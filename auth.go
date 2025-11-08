package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// CallContextInfo contains call context information from FreeSWITCH
type CallContextInfo struct {
	UUID        string
	AccountCode string
	Found       bool
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

// getCallContext fetches call context information from FreeSWITCH
func (h *APIHandler) getCallContext(callUUID string) (*CallContextInfo, error) {
	// Get all calls
	callsResponse, err := h.eslClient.SendCommand("api show calls as json")
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve calls: %v", err)
	}

	var callsData struct {
		Rows []struct {
			UUID        string `json:"uuid"`
			BUUID       string `json:"b_uuid"`
			AccountCode string `json:"accountcode"`
		} `json:"rows"`
	}

	if err := json.Unmarshal([]byte(callsResponse), &callsData); err != nil {
		return nil, fmt.Errorf("failed to parse calls data: %v", err)
	}

	// Find call by UUID (check both A-leg and B-leg)
	for _, row := range callsData.Rows {
		if row.UUID == callUUID || row.BUUID == callUUID {
			return &CallContextInfo{
				UUID:        callUUID,
				AccountCode: row.AccountCode,
				Found:       true,
			}, nil
		}
	}

	return &CallContextInfo{
		UUID:  callUUID,
		Found: false,
	}, nil
}

// validateCallContext validates that a call belongs to an allowed context
// Returns the call context info and true if valid, or responds with error and returns false
func (h *APIHandler) validateCallContext(w http.ResponseWriter, r *http.Request, callUUID string) (*CallContextInfo, bool) {
	// Check if unrestricted access
	if isUnrestrictedAccess(r) {
		// Still verify call exists for proper 404
		callInfo, err := h.getCallContext(callUUID)
		if err != nil {
			h.respondError(w, r, fmt.Sprintf("Failed to verify call: %v", err), http.StatusInternalServerError)
			return nil, false
		}
		if !callInfo.Found {
			h.respondError(w, r, fmt.Sprintf("Call %s not found", callUUID), http.StatusNotFound)
			return nil, false
		}
		return callInfo, true
	}

	allowedContexts := getAllowedContexts(r)

	// Fetch call context
	callInfo, err := h.getCallContext(callUUID)
	if err != nil {
		h.respondError(w, r, fmt.Sprintf("Failed to verify call context: %v", err), http.StatusInternalServerError)
		return nil, false
	}

	if !callInfo.Found {
		h.respondError(w, r, fmt.Sprintf("Call %s not found", callUUID), http.StatusNotFound)
		return nil, false
	}

	// Check if call context is allowed
	for _, allowed := range allowedContexts {
		if callInfo.AccountCode == allowed {
			return callInfo, true
		}
	}

	// Context not allowed
	allowedList := strings.Join(allowedContexts, ", ")
	h.respondError(w, r,
		fmt.Sprintf("Call %s belongs to context '%s' which is not in your allowed contexts: [%s]",
			callUUID, callInfo.AccountCode, allowedList),
		http.StatusForbidden)
	return nil, false
}

// validateRequestContext validates a context specified in the request body
// Returns true if valid, or responds with error and returns false
func (h *APIHandler) validateRequestContext(w http.ResponseWriter, r *http.Request, requestContext string) bool {
	// Check if unrestricted access
	if isUnrestrictedAccess(r) {
		return true
	}

	allowedContexts := getAllowedContexts(r)

	// Check if request context is allowed
	for _, allowed := range allowedContexts {
		if requestContext == allowed {
			return true
		}
	}

	// Context not allowed
	allowedList := strings.Join(allowedContexts, ", ")
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
