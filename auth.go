package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	allowedContextsKey contextKey = "allowedContexts"
	WILDCARD_CONTEXT              = "*"
)

// Context authorization structures
type contextAuth struct {
	Contexts     []string
	Unrestricted bool
}

// CallContextInfo contains call context information from FreeSWITCH, read
// from the call's own uuid_dump on the serialized connection.
type CallContextInfo struct {
	UUID        string
	AccountCode string
	Found       bool
	// Channel variables AddToConference needs; "" when unset.
	DomainName             string
	ConferenceName         string
	BridgeUUID             string
	OutboundCallerIDNumber string
}

// isUnrestrictedAccess checks if the request has unrestricted context access
func isUnrestrictedAccess(r *http.Request) bool {
	if auth, ok := r.Context().Value(allowedContextsKey).(contextAuth); ok {
		return auth.Unrestricted
	}
	return true // Default to unrestricted if not set
}

// getAllowedContexts returns the list of allowed contexts from the request
func getAllowedContexts(r *http.Request) []string {
	if auth, ok := r.Context().Value(allowedContextsKey).(contextAuth); ok {
		return auth.Contexts
	}
	return nil
}

// channelDumper runs `uuid_dump` with each reply matched to its request
// (eslSerialClient): a dump for another call must never decide a call's
// authorization or be returned as its details.
type channelDumper interface {
	ChannelDump(ctx context.Context, callUUID string) (map[string]any, error)
}

// channelDumpTimeout bounds one uuid_dump, including waiting for the serialized connection.
const channelDumpTimeout = 10 * time.Second

// getCallContext fetches call context information from FreeSWITCH
func (h *APIHandler) getCallContext(callUUID string) (*CallContextInfo, error) {
	// FreeSWITCH call uuids are canonical lowercase; anything else names no channel.
	if !canonicalUUID.MatchString(callUUID) {
		return &CallContextInfo{UUID: callUUID, Found: false}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), channelDumpTimeout)
	defer cancel()
	dumpData, err := h.channelDumps.ChannelDump(ctx, callUUID)
	if errors.Is(err, errChannelGone) {
		return &CallContextInfo{UUID: callUUID, Found: false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve call: %v", err)
	}
	return contextFromDump(callUUID, dumpData), nil
}

// contextFromDump reads a call's tenant and the variables fs-api needs from
// its uuid_dump.
func contextFromDump(callUUID string, dumpData map[string]any) *CallContextInfo {
	// Determine context: prefer variable_accountcode, then Caller-Context, then variable_domain_name
	callContext := ""
	if v, ok := dumpData["variable_accountcode"].(string); ok && v != "" {
		callContext = v
	} else if v, ok := dumpData["Caller-Context"].(string); ok && v != "" {
		callContext = v
	} else if v, ok := dumpData["variable_domain_name"].(string); ok && v != "" {
		callContext = v
	}

	str := func(key string) string {
		v, _ := dumpData[key].(string)
		return strings.TrimSpace(v)
	}
	return &CallContextInfo{
		UUID:                   callUUID,
		AccountCode:            callContext,
		Found:                  true,
		DomainName:             str("variable_domain_name"),
		ConferenceName:         str("variable_conference_name"),
		BridgeUUID:             str("variable_bridge_uuid"),
		OutboundCallerIDNumber: str("variable_outbound_caller_id_number"),
	}
}

// validateCallContext validates that a call belongs to an allowed context
// Returns the call context info and true if valid, or responds with error and returns false
func (h *APIHandler) validateCallContext(w http.ResponseWriter, r *http.Request, callUUID string) (*CallContextInfo, bool) {
	callInfo, err := h.getCallContext(callUUID)
	if err != nil {
		h.respondError(w, r, fmt.Sprintf("Failed to verify call context: %v", err), http.StatusInternalServerError)
		return nil, false
	}
	if !callInfo.Found {
		h.respondError(w, r, fmt.Sprintf("Call %s not found", callUUID), http.StatusNotFound)
		return nil, false
	}
	if !contextAllowed(r, callInfo.AccountCode) {
		h.respondError(w, r,
			fmt.Sprintf("Call %s belongs to context '%s' which is not in your allowed contexts: [%s]",
				callUUID, callInfo.AccountCode, strings.Join(getAllowedContexts(r), ", ")),
			http.StatusForbidden)
		return nil, false
	}
	return callInfo, true
}

// sameTenant: two legs of one tenant share its accountcode, or (for an
// extension with its own accountcode) its domain_name.
func sameTenant(a, b *CallContextInfo) bool {
	return a.AccountCode == b.AccountCode || (a.DomainName != "" && a.DomainName == b.DomainName)
}

// validateRequestContext validates a context specified in the request body
// Returns true if valid, or responds with error and returns false
func (h *APIHandler) validateRequestContext(w http.ResponseWriter, r *http.Request, requestContext string) bool {
	if contextAllowed(r, requestContext) {
		return true
	}
	allowedList := strings.Join(getAllowedContexts(r), ", ")
	h.respondError(w, r,
		fmt.Sprintf("Cannot originate call in context '%s' - not in your allowed contexts: [%s]",
			requestContext, allowedList),
		http.StatusForbidden)
	return false
}

// contextAuthMiddleware extracts X-Allowed-Contexts header and stores in request context
func contextAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowedContextsHeader := r.Header.Get("X-Allowed-Contexts")

		var allowedContexts []string
		isUnrestricted := false

		if allowedContextsHeader == "" {
			// No header = unrestricted (backward compatibility)
			isUnrestricted = true
		} else {
			// Parse comma-separated contexts
			contexts := strings.Split(allowedContextsHeader, ",")
			for _, ctx := range contexts {
				trimmed := strings.TrimSpace(ctx)
				if trimmed == "" {
					continue
				}
				if trimmed == WILDCARD_CONTEXT {
					// Wildcard found = unrestricted
					isUnrestricted = true
					break
				}
				allowedContexts = append(allowedContexts, trimmed)
			}
		}

		// Store both the list and unrestricted flag
		auth := contextAuth{
			Contexts:     allowedContexts,
			Unrestricted: isUnrestricted,
		}

		ctx := context.WithValue(r.Context(), allowedContextsKey, auth)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
