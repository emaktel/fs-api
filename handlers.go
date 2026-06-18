package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

// Context keys
type contextKey string

const requestIDKey contextKey = "requestID"

func getRequestID(r *http.Request) string {
	if reqID, ok := r.Context().Value(requestIDKey).(string); ok {
		return reqID
	}
	return "unknown"
}

// API Handlers
type APIHandler struct {
	eslClient ESLClient
	eventSubscriber *EventSubscriber
}

func NewAPIHandler(eslHost, eslPort, eslPassword string) *APIHandler {
	return &APIHandler{
		eslClient: NewESLClient(eslHost, eslPort, eslPassword),
	}
}

func (h *APIHandler) respondSuccess(w http.ResponseWriter, r *http.Request, message string) {
	requestID := getRequestID(r)
	logInfo(requestID, message)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(SuccessResponse{
		Status:  "success",
		Message: message,
	})
}

func (h *APIHandler) respondError(w http.ResponseWriter, r *http.Request, message string, statusCode int) {
	requestID := getRequestID(r)

	if statusCode >= 500 {
		logError(requestID, message, nil)
	} else {
		logWarn(requestID, message)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(ErrorResponse{
		Status:  "error",
		Message: message,
	})
}

// Helper to determine appropriate HTTP status code based on error
func (h *APIHandler) getErrorStatusCode(err error) int {
	if err == nil {
		return http.StatusOK
	}

	errMsg := err.Error()

	// ESL connection errors -> Service Unavailable
	if strings.Contains(errMsg, "ESL connection failed") {
		return http.StatusServiceUnavailable
	}

	// ESL command errors -> Bad Gateway (upstream service error)
	if strings.Contains(errMsg, "ESL error") || strings.Contains(errMsg, "-ERR") {
		return http.StatusBadGateway
	}

	// Default to Internal Server Error for unknown errors
	return http.StatusInternalServerError
}

// POST /v1/calls/{uuid}/hangup
func (h *APIHandler) HangupCall(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	callUUID := vars["uuid"]

	// Validate UUID
	if err := validateUUID(callUUID); err != nil {
		h.respondError(w, r, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate call context
	if _, ok := h.validateCallContext(w, r, callUUID); !ok {
		return
	}

	var req HangupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Use default cause if no body provided
		req.Cause = "NORMAL_CLEARING"
	}

	if req.Cause == "" {
		req.Cause = "NORMAL_CLEARING"
	}

	cmd := fmt.Sprintf("api uuid_kill %s %s", callUUID, req.Cause)
	_, err := h.eslClient.SendCommand(cmd)
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to hangup call: %v", err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Call %s hung up with cause %s", callUUID, req.Cause))
}

// POST /v1/calls/{uuid}/transfer
func (h *APIHandler) TransferCall(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	callUUID := vars["uuid"]

	// Validate UUID
	if err := validateUUID(callUUID); err != nil {
		h.respondError(w, r, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate call context
	if _, ok := h.validateCallContext(w, r, callUUID); !ok {
		return
	}

	var req TransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, r, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Only destination is required
	if req.Destination == "" {
		h.respondError(w, r, "destination is required", http.StatusBadRequest)
		return
	}

	// Default to "aleg" if not specified
	if req.Leg == "" {
		req.Leg = "aleg"
	}

	// Validate leg parameter
	leg := strings.ToLower(req.Leg)
	if leg != "aleg" && leg != "bleg" && leg != "both" {
		h.respondError(w, r, "leg must be 'aleg', 'bleg', or 'both'", http.StatusBadRequest)
		return
	}

	// Build the command: uuid_transfer <uuid> [-bleg|-both] <dest-exten> [<dialplan>] [<context>]
	var cmd strings.Builder
	cmd.WriteString("api uuid_transfer ")
	cmd.WriteString(callUUID)
	cmd.WriteString(" ")

	// Add optional flag (-bleg or -both)
	var legType string
	if leg == "bleg" {
		cmd.WriteString("-bleg ")
		legType = "B-leg"
	} else if leg == "both" {
		cmd.WriteString("-both ")
		legType = "both legs"
	} else {
		legType = "A-leg"
	}

	// Add destination (required)
	cmd.WriteString(req.Destination)

	// Add dialplan and context as a pair (both or neither)
	// If context is provided, dialplan defaults to "XML"
	if req.Context != "" {
		dialplan := req.Dialplan
		if dialplan == "" {
			dialplan = "XML"
		}
		cmd.WriteString(" ")
		cmd.WriteString(dialplan)
		cmd.WriteString(" ")
		cmd.WriteString(req.Context)
	}

	_, err := h.eslClient.SendCommand(cmd.String())
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to transfer call: %v", err), statusCode)
		return
	}

	// Build success message
	var message strings.Builder
	message.WriteString(fmt.Sprintf("Call %s (%s) transferred to %s", callUUID, legType, req.Destination))
	if req.Dialplan != "" {
		message.WriteString(fmt.Sprintf(" dialplan %s", req.Dialplan))
	}
	if req.Context != "" {
		message.WriteString(fmt.Sprintf(" context %s", req.Context))
	}

	h.respondSuccess(w, r, message.String())
}

// POST /v1/calls/bridge
func (h *APIHandler) BridgeCalls(w http.ResponseWriter, r *http.Request) {
	var req BridgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, r, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.UUIDA == "" || req.UUIDB == "" {
		h.respondError(w, r, "uuid_a and uuid_b are required", http.StatusBadRequest)
		return
	}

	// Validate both UUIDs
	if err := validateUUID(req.UUIDA); err != nil {
		h.respondError(w, r, fmt.Sprintf("uuid_a: %v", err), http.StatusBadRequest)
		return
	}
	if err := validateUUID(req.UUIDB); err != nil {
		h.respondError(w, r, fmt.Sprintf("uuid_b: %v", err), http.StatusBadRequest)
		return
	}

	// Validate both call contexts
	if _, ok := h.validateCallContext(w, r, req.UUIDA); !ok {
		return
	}
	if _, ok := h.validateCallContext(w, r, req.UUIDB); !ok {
		return
	}

	cmd := fmt.Sprintf("api uuid_bridge %s %s", req.UUIDA, req.UUIDB)
	_, err := h.eslClient.SendCommand(cmd)
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to bridge calls: %v", err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Calls %s and %s bridged", req.UUIDA, req.UUIDB))
}

// POST /v1/calls/{uuid}/answer
func (h *APIHandler) AnswerCall(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	callUUID := vars["uuid"]

	// Validate UUID
	if err := validateUUID(callUUID); err != nil {
		h.respondError(w, r, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate call context
	if _, ok := h.validateCallContext(w, r, callUUID); !ok {
		return
	}

	cmd := fmt.Sprintf("api uuid_answer %s", callUUID)
	_, err := h.eslClient.SendCommand(cmd)
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to answer call: %v", err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Call %s answered", callUUID))
}

// POST /v1/calls/{uuid}/hold
func (h *APIHandler) ControlHold(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	callUUID := vars["uuid"]

	// Validate UUID
	if err := validateUUID(callUUID); err != nil {
		h.respondError(w, r, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate call context
	if _, ok := h.validateCallContext(w, r, callUUID); !ok {
		return
	}

	var req HoldRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, r, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Action != "hold" && req.Action != "unhold" {
		h.respondError(w, r, "action must be 'hold' or 'unhold'", http.StatusBadRequest)
		return
	}

	var cmd string
	if req.Action == "hold" {
		cmd = fmt.Sprintf("api uuid_hold %s", callUUID)
	} else {
		cmd = fmt.Sprintf("api uuid_hold off %s", callUUID)
	}

	_, err := h.eslClient.SendCommand(cmd)
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to %s call: %v", req.Action, err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Call %s %s", callUUID, req.Action))
}

// POST /v1/calls/{uuid}/record
func (h *APIHandler) ControlRecording(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	callUUID := vars["uuid"]

	// Validate UUID
	if err := validateUUID(callUUID); err != nil {
		h.respondError(w, r, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate call context
	if _, ok := h.validateCallContext(w, r, callUUID); !ok {
		return
	}

	var req RecordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, r, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Action != "start" && req.Action != "stop" {
		h.respondError(w, r, "action must be 'start' or 'stop'", http.StatusBadRequest)
		return
	}

	var cmd string
	if req.Action == "start" {
		if req.Filename == "" {
			h.respondError(w, r, "filename is required for start action", http.StatusBadRequest)
			return
		}
		// Validate file path
		if err := validateFilePath(req.Filename); err != nil {
			h.respondError(w, r, fmt.Sprintf("Invalid filename: %v", err), http.StatusBadRequest)
			return
		}
		cmd = fmt.Sprintf("api uuid_record %s start %s", callUUID, req.Filename)
	} else {
		cmd = fmt.Sprintf("api uuid_record %s stop all", callUUID)
	}

	_, err := h.eslClient.SendCommand(cmd)
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to %s recording: %v", req.Action, err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Recording %s for call %s", req.Action, callUUID))
}

// POST /v1/calls/{uuid}/dtmf
func (h *APIHandler) SendDTMF(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	callUUID := vars["uuid"]

	// Validate UUID
	if err := validateUUID(callUUID); err != nil {
		h.respondError(w, r, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate call context
	if _, ok := h.validateCallContext(w, r, callUUID); !ok {
		return
	}

	var req DTMFRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, r, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Digits == "" {
		h.respondError(w, r, "digits are required", http.StatusBadRequest)
		return
	}

	duration := req.Duration
	if duration == 0 {
		duration = 100
	}

	cmd := fmt.Sprintf("api uuid_send_dtmf %s %s@%d", callUUID, req.Digits, duration)
	_, err := h.eslClient.SendCommand(cmd)
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to send DTMF: %v", err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("DTMF %s sent to call %s", req.Digits, callUUID))
}

// POST /v1/calls/{uuid}/park
func (h *APIHandler) ParkCall(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	callUUID := vars["uuid"]

	// Validate UUID
	if err := validateUUID(callUUID); err != nil {
		h.respondError(w, r, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate call context
	if _, ok := h.validateCallContext(w, r, callUUID); !ok {
		return
	}

	cmd := fmt.Sprintf("api uuid_park %s", callUUID)
	_, err := h.eslClient.SendCommand(cmd)
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to park call: %v", err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Call %s parked", callUUID))
}

// POST /v1/calls/originate
func (h *APIHandler) OriginateCall(w http.ResponseWriter, r *http.Request) {
	requestID := getRequestID(r)

	var req OriginateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, r, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate required fields
	if req.ALeg == "" {
		h.respondError(w, r, "aleg is required", http.StatusBadRequest)
		return
	}

	// Validate context if provided
	if req.Context != "" {
		if !h.validateRequestContext(w, r, req.Context) {
			return
		}
	}

	// If bleg is not provided, default to park
	if req.BLeg == "" {
		req.BLeg = "&park()"
	}

	// Build channel variables string
	// Start with user-provided channel variables
	vars := []string{}
	if len(req.ChannelVariables) > 0 {
		for key, value := range req.ChannelVariables {
			switch v := value.(type) {
			case string:
				vars = append(vars, fmt.Sprintf("%s=%s", key, v))
			case bool:
				vars = append(vars, fmt.Sprintf("%s=%t", key, v))
			case float64:
				vars = append(vars, fmt.Sprintf("%s=%v", key, v))
			default:
				vars = append(vars, fmt.Sprintf("%s=%v", key, v))
			}
		}
	}

	// Add caller ID as channel variables (these take precedence)
	if req.CallerIDNumber != "" {
		vars = append(vars, fmt.Sprintf("origination_caller_id_number=%s", req.CallerIDNumber))
	}
	if req.CallerIDName != "" {
		// Quote caller ID name in case it contains spaces
		vars = append(vars, fmt.Sprintf("origination_caller_id_name='%s'", req.CallerIDName))
	}

	var channelVars string
	if len(vars) > 0 {
		channelVars = fmt.Sprintf("{%s}", strings.Join(vars, ","))
	}

	// Build the originate command: originate {vars}aleg bleg [dialplan] [context] [cid_name] [cid_num] [timeout]
	var cmd strings.Builder
	cmd.WriteString("api originate ")

	// Add channel variables if present
	if channelVars != "" {
		cmd.WriteString(channelVars)
	}

	// Add A-leg
	cmd.WriteString(req.ALeg)
	cmd.WriteString(" ")

	// Add B-leg (can be extension or &application)
	cmd.WriteString(req.BLeg)

	// Add optional parameters in order: dialplan, context, cid_name, cid_num, timeout
	// Note: When using channel variables for caller ID (origination_caller_id_*),
	// we include them here only if NOT already in the channel variables

	if req.Dialplan != "" {
		cmd.WriteString(" ")
		cmd.WriteString(req.Dialplan)
	}

	if req.Context != "" {
		cmd.WriteString(" ")
		cmd.WriteString(req.Context)
	}

	// Add cid_name or skip it
	if req.CallerIDName != "" && !strings.Contains(channelVars, "origination_caller_id_name") {
		cmd.WriteString(" ")
		cmd.WriteString(req.CallerIDName)
	}

	// Add cid_num or skip it
	if req.CallerIDNumber != "" && !strings.Contains(channelVars, "origination_caller_id_number") {
		cmd.WriteString(" ")
		cmd.WriteString(req.CallerIDNumber)
	}

	// Add timeout if specified
	if req.TimeoutSec > 0 {
		cmd.WriteString(" ")
		cmd.WriteString(fmt.Sprintf("%d", req.TimeoutSec))
	}

	// Send the originate command
	response, err := h.eslClient.SendCommand(cmd.String())
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to originate call: %v", err), statusCode)
		return
	}

	logInfo(requestID, "Call originated successfully")

	// Parse call UUID from response (format: "+OK <uuid>")
	response = strings.TrimSpace(response)
	if strings.HasPrefix(response, "+OK ") {
		response = strings.TrimPrefix(response, "+OK ")
		response = strings.TrimPrefix(response, "Job-UUID: ")
	}
	parsedCallUUID := strings.TrimSpace(response)

	// Register call for event tracking
	if h.eventSubscriber != nil && parsedCallUUID != "" {
		userUUID := r.Header.Get("X-User-UUID")
		domainUUID := r.Header.Get("X-Domain-UUID")
		allowedContexts := getAllowedContexts(r)
		domainName := ""
		if len(allowedContexts) > 0 {
			domainName = allowedContexts[0]
		}
		h.eventSubscriber.RegisterCall(&CallRegistration{
			CallUUID:    parsedCallUUID,
			UserUUID:    userUUID,
			DomainUUID:  domainUUID,
			DomainName:  domainName,
			CallbackURL: req.CallbackURL,
			CreatedAt:   time.Now(),
		})
	}

	// Return the response (usually contains job UUID or call UUID)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"response": strings.TrimSpace(response),
		},
	})
}

// channelOnlyFields are fields returned by `show channels` that are not present
// in `show calls`. Used to enrich /v1/calls responses without overwriting any
// key that `show calls` already provides.
var channelOnlyFields = []string{
	"application", "application_data",
	"dialplan", "context",
	"read_codec", "read_rate", "read_bit_rate",
	"write_codec", "write_rate", "write_bit_rate",
	"secure",
	"initial_cid_name", "initial_cid_num", "initial_ip_addr",
	"initial_dest", "initial_dialplan", "initial_context",
}

// enrichCallWithChannel copies channel-only fields from ch into call. When bLeg
// is true, field names are prefixed with "b_" to match the show-calls convention.
func enrichCallWithChannel(call, ch map[string]interface{}, bLeg bool) {
	prefix := ""
	if bLeg {
		prefix = "b_"
	}
	for _, f := range channelOnlyFields {
		if v, ok := ch[f]; ok {
			call[prefix+f] = v
		}
	}
}

// GET /v1/calls
func (h *APIHandler) ListCalls(w http.ResponseWriter, r *http.Request) {
	requestID := getRequestID(r)

	// Check if X-Allowed-Contexts header is present
	allowedContextsHeader := r.Header.Get("X-Allowed-Contexts")
	if allowedContextsHeader == "" {
		h.respondError(w, r, "X-Allowed-Contexts header is required for this endpoint", http.StatusBadRequest)
		return
	}

	// Get allowed contexts from the middleware
	allowedContexts := getAllowedContexts(r)
	unrestricted := isUnrestrictedAccess(r)

	// Step 1: Get all calls from FreeSWITCH
	callsResponse, err := h.eslClient.SendCommand("api show calls as json")
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to retrieve calls: %v", err), statusCode)
		return
	}

	// Step 2: Parse JSON response
	var callsData struct {
		RowCount int                      `json:"row_count"`
		Rows     []map[string]interface{} `json:"rows"`
	}

	if err := json.Unmarshal([]byte(callsResponse), &callsData); err != nil {
		h.respondError(w, r, fmt.Sprintf("Failed to parse calls data: %v", err), http.StatusInternalServerError)
		return
	}

	// Step 3: Fetch channels for context fallback (restricted path) and field
	// enrichment. Non-fatal on error: calls are still returned, just without
	// channel-only fields merged in.
	channelsByUUID := map[string]map[string]interface{}{}
	if len(callsData.Rows) > 0 {
		channelsResponse, chErr := h.eslClient.SendCommand("api show channels as json")
		if chErr == nil {
			var channelsData struct {
				Rows []map[string]interface{} `json:"rows"`
			}
			if json.Unmarshal([]byte(channelsResponse), &channelsData) == nil {
				for _, ch := range channelsData.Rows {
					if uuid, _ := ch["uuid"].(string); uuid != "" {
						channelsByUUID[uuid] = ch
					}
				}
			}
		}
	}

	// Step 4: Filter calls based on allowed contexts
	var filteredCalls []map[string]interface{}

	if unrestricted {
		// Wildcard or no restrictions - return all calls
		filteredCalls = callsData.Rows
		logInfo(requestID, fmt.Sprintf("Retrieved all calls (unrestricted access): %d calls", len(filteredCalls)))
	} else {
		for _, call := range callsData.Rows {
			// Prefer accountcode, fall back to channel context
			callContext, _ := call["accountcode"].(string)
			if callContext == "" {
				if uuid, _ := call["uuid"].(string); uuid != "" {
					if ch, ok := channelsByUUID[uuid]; ok {
						callContext, _ = ch["context"].(string)
					}
				}
			}
			if callContext == "" {
				continue
			}

			// Check if this call's context is in the allowed list
			for _, allowed := range allowedContexts {
				if callContext == allowed {
					filteredCalls = append(filteredCalls, call)
					break
				}
			}
		}
		logInfo(requestID, fmt.Sprintf("Retrieved filtered calls for contexts %v: %d calls", allowedContexts, len(filteredCalls)))
	}

	// Step 5: Enrich each surviving call with channel-only fields for the A-leg
	// (uuid) and, when present, the B-leg (b_uuid). Missing channels are
	// tolerated — enrichment is skipped for that leg.
	for _, call := range filteredCalls {
		if uuid, _ := call["uuid"].(string); uuid != "" {
			if ch, ok := channelsByUUID[uuid]; ok {
				enrichCallWithChannel(call, ch, false)
			}
		}
		if bUUID, _ := call["b_uuid"].(string); bUUID != "" {
			if ch, ok := channelsByUUID[bUUID]; ok {
				enrichCallWithChannel(call, ch, true)
			}
		}
	}

	// Step 6: Return the filtered calls
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "success",
		"row_count": len(filteredCalls),
		"rows":      filteredCalls,
	})
}

// GET /v1/calls/{uuid}
func (h *APIHandler) GetCallDetails(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	callUUID := vars["uuid"]
	requestID := getRequestID(r)

	// Validate UUID
	if err := validateUUID(callUUID); err != nil {
		h.respondError(w, r, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate call context (this also checks if call exists)
	if _, ok := h.validateCallContext(w, r, callUUID); !ok {
		return
	}

	// Step 1: Get call information to extract both A-leg and B-leg UUIDs
	// Note: FreeSWITCH "show calls" doesn't support WHERE clause, so we get all calls and filter
	showCallsCmd := "api show calls as json"
	callsResponse, err := h.eslClient.SendCommand(showCallsCmd)
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to retrieve call information: %v", err), statusCode)
		return
	}

	// Step 2: Parse JSON response to extract UUIDs
	var callsData struct {
		RowCount int `json:"row_count"`
		Rows     []struct {
			UUID  string `json:"uuid"`
			BUUID string `json:"b_uuid"`
		} `json:"rows"`
	}

	if err := json.Unmarshal([]byte(callsResponse), &callsData); err != nil {
		h.respondError(w, r, fmt.Sprintf("Failed to parse call information: %v", err), http.StatusInternalServerError)
		return
	}

	// Find the specific call by UUID (check both A-leg and B-leg UUIDs)
	var aLegUUID, bLegUUID string
	var callFound bool
	for _, row := range callsData.Rows {
		if row.UUID == callUUID {
			// Input UUID matches A-leg
			aLegUUID = row.UUID
			bLegUUID = row.BUUID
			callFound = true
			break
		} else if row.BUUID == callUUID {
			// Input UUID matches B-leg
			aLegUUID = row.UUID
			bLegUUID = row.BUUID
			callFound = true
			break
		}
	}

	// Check if call was found
	if !callFound {
		h.respondError(w, r, fmt.Sprintf("Call %s not found", callUUID), http.StatusNotFound)
		return
	}

	// Step 3: Dump A-leg details as JSON
	aLegDumpCmd := fmt.Sprintf("api uuid_dump %s json", aLegUUID)
	aLegDetailsStr, err := h.eslClient.SendCommand(aLegDumpCmd)
	if err != nil {
		logWarn(requestID, fmt.Sprintf("Failed to retrieve A-leg details: %v", err))
		h.respondError(w, r, fmt.Sprintf("Failed to retrieve A-leg details: %v", err), http.StatusInternalServerError)
		return
	}

	// Parse A-leg JSON
	var aLegDetails map[string]interface{}
	if err := json.Unmarshal([]byte(aLegDetailsStr), &aLegDetails); err != nil {
		logWarn(requestID, fmt.Sprintf("Failed to parse A-leg details: %v", err))
		h.respondError(w, r, fmt.Sprintf("Failed to parse A-leg details: %v", err), http.StatusInternalServerError)
		return
	}

	// Step 4: Dump B-leg details (if B-leg exists)
	var bLegDetails map[string]interface{}
	if bLegUUID != "" {
		bLegDumpCmd := fmt.Sprintf("api uuid_dump %s json", bLegUUID)
		bLegDetailsStr, err := h.eslClient.SendCommand(bLegDumpCmd)
		if err != nil {
			logWarn(requestID, fmt.Sprintf("Failed to retrieve B-leg details: %v", err))
			// B-leg might not exist anymore, this is not fatal
			bLegDetails = nil
		} else {
			if err := json.Unmarshal([]byte(bLegDetailsStr), &bLegDetails); err != nil {
				logWarn(requestID, fmt.Sprintf("Failed to parse B-leg details: %v", err))
				bLegDetails = nil
			}
		}
	}

	// Parse call_info JSON and extract the first row
	var callInfoWrapper struct {
		RowCount int                      `json:"row_count"`
		Rows     []map[string]interface{} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(callsResponse), &callInfoWrapper); err != nil {
		logWarn(requestID, fmt.Sprintf("Failed to parse call info: %v", err))
		h.respondError(w, r, fmt.Sprintf("Failed to parse call info: %v", err), http.StatusInternalServerError)
		return
	}

	// Validate that we got data (we already validated the call exists)
	if len(callInfoWrapper.Rows) == 0 {
		h.respondError(w, r, "Call data not found in response", http.StatusInternalServerError)
		return
	}

	// Enrich call_info with channel-only fields so it matches the /v1/calls
	// list response shape. Non-fatal on error: the rest of the response still
	// includes full A-leg/B-leg uuid_dump data.
	if channelsResponse, chErr := h.eslClient.SendCommand("api show channels as json"); chErr == nil {
		var channelsData struct {
			Rows []map[string]interface{} `json:"rows"`
		}
		if json.Unmarshal([]byte(channelsResponse), &channelsData) == nil {
			channelsByUUID := map[string]map[string]interface{}{}
			for _, ch := range channelsData.Rows {
				if uuid, _ := ch["uuid"].(string); uuid != "" {
					channelsByUUID[uuid] = ch
				}
			}
			if ch, ok := channelsByUUID[aLegUUID]; ok {
				enrichCallWithChannel(callInfoWrapper.Rows[0], ch, false)
			}
			if bLegUUID != "" {
				if ch, ok := channelsByUUID[bLegUUID]; ok {
					enrichCallWithChannel(callInfoWrapper.Rows[0], ch, true)
				}
			}
		}
	}

	logInfo(requestID, fmt.Sprintf("Call details retrieved for %s", callUUID))

	// Return the complete call information with clean structure
	// Note: We build the response manually to control field ordering in JSON output
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)

	// Build response with ordered keys: status, call_info, aleg (uuid then details), bleg (uuid then details)
	var responseJSON strings.Builder
	responseJSON.WriteString(`{"status":"success","call_info":`)

	// Just use call_info as-is from FreeSWITCH (preserves their ordering)
	callInfoJSON, _ := json.Marshal(callInfoWrapper.Rows[0])
	responseJSON.Write(callInfoJSON)

	responseJSON.WriteString(`,"aleg":{"uuid":"`)
	responseJSON.WriteString(aLegUUID)
	responseJSON.WriteString(`","details":`)
	aLegJSON, _ := json.Marshal(aLegDetails)
	responseJSON.Write(aLegJSON)
	responseJSON.WriteString(`}`)

	if bLegUUID != "" {
		responseJSON.WriteString(`,"bleg":{"uuid":"`)
		responseJSON.WriteString(bLegUUID)
		responseJSON.WriteString(`","details":`)
		bLegJSON, _ := json.Marshal(bLegDetails)
		responseJSON.Write(bLegJSON)
		responseJSON.WriteString(`}`)
	}

	responseJSON.WriteString(`}`)

	w.Write([]byte(responseJSON.String()))
}

// GET /v1/status
func (h *APIHandler) GetStatus(w http.ResponseWriter, r *http.Request) {
	requestID := getRequestID(r)

	// Send status command to FreeSWITCH using JSON format
	response, err := h.eslClient.SendCommand(`api json {"command":"status","data":""}`)
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to get FreeSWITCH status: %v", err), statusCode)
		return
	}

	logInfo(requestID, "FreeSWITCH status retrieved successfully")

	// Parse the JSON response from FreeSWITCH
	var fsResponse map[string]interface{}
	if err := json.Unmarshal([]byte(response), &fsResponse); err != nil {
		// If response is not JSON, return error
		h.respondError(w, r, fmt.Sprintf("Failed to parse FreeSWITCH JSON response: %v", err), http.StatusInternalServerError)
		return
	}

	// Extract just the "response" field from FreeSWITCH's JSON response
	responseData, ok := fsResponse["response"]
	if !ok {
		h.respondError(w, r, "FreeSWITCH response missing 'response' field", http.StatusInternalServerError)
		return
	}

	// Return clean response structure
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"data":   responseData,
	})
}

// GET /v1/registrations
func (h *APIHandler) ListRegistrations(w http.ResponseWriter, r *http.Request) {
	requestID := getRequestID(r)

	// X-Allowed-Contexts header is required
	if r.Header.Get("X-Allowed-Contexts") == "" {
		h.respondError(w, r, "X-Allowed-Contexts header is required for this endpoint", http.StatusBadRequest)
		return
	}

	allowedContexts := getAllowedContexts(r)
	unrestricted := isUnrestrictedAccess(r)

	response, err := h.eslClient.SendCommand("api show registrations as json")
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to retrieve registrations: %v", err), statusCode)
		return
	}

	var regsData struct {
		RowCount int                      `json:"row_count"`
		Rows     []map[string]interface{} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(response), &regsData); err != nil {
		h.respondError(w, r, fmt.Sprintf("Failed to parse registrations data: %v", err), http.StatusInternalServerError)
		return
	}

	var filtered []map[string]interface{}
	if unrestricted {
		filtered = regsData.Rows
		logInfo(requestID, fmt.Sprintf("Retrieved all registrations (unrestricted): %d", len(filtered)))
	} else {
		for _, reg := range regsData.Rows {
			realm, _ := reg["realm"].(string)
			for _, allowed := range allowedContexts {
				if realm == allowed {
					filtered = append(filtered, reg)
					break
				}
			}
		}
		logInfo(requestID, fmt.Sprintf("Retrieved filtered registrations for contexts %v: %d", allowedContexts, len(filtered)))
	}

	if filtered == nil {
		filtered = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "success",
		"row_count": len(filtered),
		"rows":      filtered,
	})
}

// GET /v1/registrations/count
func (h *APIHandler) CountRegistrations(w http.ResponseWriter, r *http.Request) {
	requestID := getRequestID(r)

	// X-Allowed-Contexts header is required
	if r.Header.Get("X-Allowed-Contexts") == "" {
		h.respondError(w, r, "X-Allowed-Contexts header is required for this endpoint", http.StatusBadRequest)
		return
	}

	allowedContexts := getAllowedContexts(r)
	unrestricted := isUnrestrictedAccess(r)

	response, err := h.eslClient.SendCommand("api show registrations as json")
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to retrieve registrations: %v", err), statusCode)
		return
	}

	var regsData struct {
		RowCount int `json:"row_count"`
		Rows     []struct {
			Realm string `json:"realm"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(response), &regsData); err != nil {
		h.respondError(w, r, fmt.Sprintf("Failed to parse registrations data: %v", err), http.StatusInternalServerError)
		return
	}

	count := 0
	if unrestricted {
		count = regsData.RowCount
	} else {
		for _, reg := range regsData.Rows {
			for _, allowed := range allowedContexts {
				if reg.Realm == allowed {
					count++
					break
				}
			}
		}
	}

	logInfo(requestID, fmt.Sprintf("Registration count for contexts %v: %d", allowedContexts, count))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
		"count":  count,
	})
}

// GET /health
func (h *APIHandler) HealthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Try to send a simple command to test ESL connection
	_, err := h.eslClient.SendCommand("api status")
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "unhealthy",
			"error":   "ESL connection unavailable",
			"version": Version,
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "healthy",
		"version": Version,
	})
}

// sanitizeDialDestination keeps only FreeSWITCH-safe dial characters (digits, +, *, #).
// Defense-in-depth: the destination is interpolated into an `api conference … bgdial`
// command, so any whitespace/quoting that could inject extra arguments is stripped. Returns
// "" when nothing dialable remains or the value is over-long.
func sanitizeDialDestination(raw string) string {
	var b strings.Builder
	for _, c := range raw {
		if (c >= '0' && c <= '9') || c == '+' || c == '*' || c == '#' {
			b.WriteRune(c)
		}
	}
	s := b.String()
	if len(s) == 0 || len(s) > 20 {
		return ""
	}
	return s
}

// channelVar fetches a single channel variable, trimmed. Returns ("", nil) when the variable
// is simply unset (FreeSWITCH "_undef_"), and a non-nil error on an ESL/command failure or a
// FreeSWITCH "-ERR" (e.g. the channel is gone). Callers MUST distinguish "not set" from
// "couldn't read" — for conference_name/bridge_uuid, treating a read error as "not conferenced"
// would tear a live bridge or skip the partner move (SP-L12).
func (h *APIHandler) channelVar(uuid, name string) (string, error) {
	resp, err := h.eslClient.SendCommand(fmt.Sprintf("api uuid_getvar %s %s", uuid, name))
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(resp)
	if strings.HasPrefix(v, "-ERR") {
		return "", fmt.Errorf("uuid_getvar %s %s failed: %s", uuid, name, v)
	}
	if v == "_undef_" {
		return "", nil
	}
	return v, nil
}

// sanitizeTollAllow keeps only characters valid in a toll_allow class list (alphanumerics,
// comma, underscore, dash) so the value can't break out of the channel-variable syntax it's
// interpolated into.
func sanitizeTollAllow(raw string) string {
	var b strings.Builder
	for _, c := range raw {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == ',' || c == '_' || c == '-' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// waitForConferenceJoin polls until every leg reports it has joined `room`. mod_conference sets
// the channel variable conference_name on a member only AFTER the async join completes, so a
// single immediate read races it; this polls (~150ms × 30 ≈ 4.5s budget) and returns true once
// all legs are in, or false if the join never completes (profile missing/disabled, leg dropped).
func (h *APIHandler) waitForConferenceJoin(legs []string, room string) bool {
	const attempts = 30
	const interval = 150 * time.Millisecond
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(interval)
		}
		allJoined := true
		for _, leg := range legs {
			name, err := h.channelVar(leg, "conference_name")
			if err != nil || name != room {
				allJoined = false
				break
			}
		}
		if allJoined {
			return true
		}
	}
	return false
}

// sanitizeCallerIDNumber keeps only valid caller-ID-number characters (digits and a leading
// +). This both prevents argument injection and avoids spaces, which would break the bgdial
// dialstring parsing.
func sanitizeCallerIDNumber(raw string) string {
	var b strings.Builder
	for _, c := range raw {
		if (c >= '0' && c <= '9') || c == '+' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// buildConferenceDialString builds the `conference … bgdial` participant dial string. The
// loopback re-enters the call's own dialplan context, so internal/external routing and gateways
// are handled exactly like a normal outbound call. Two vars are injected so the dialplan behaves
// as if the originating extension placed the call:
//   - toll_allow: the dialplan applies the SAME per-route toll gating a direct call would
//     (SP-H2 toll-fraud parity).
//   - outbound_caller_id_number: the originating extension's outbound DID, so the FusionPBX
//     outbound route's `effective_caller_id_number=${outbound_caller_id_number}` presents a valid
//     DID instead of the conference placeholder 0000000000 (which carriers 503).
//
// CRITICAL: both vars must reach the loopback leg that RUNS the dialplan, not just the leg that
// returns to the conference. The dialstring `[vars]` apply to the originate (conference-facing)
// leg, so we additionally set `loopback_export` — mod_loopback copies the listed vars onto the
// dialplan leg. The alternate var delimiter (`^^:`) keeps comma-containing values (toll_allow,
// the loopback_export list) intact. The caller-ID NAME is intentionally omitted: it can contain
// spaces (which break the bgdial dialstring), and PSTN CNAM is resolved by the terminating
// carrier from the number anyway.
func buildConferenceDialString(tollAllow, cidNum, context, dest string) string {
	var vars, exports []string
	if t := sanitizeTollAllow(tollAllow); t != "" {
		vars = append(vars, "toll_allow="+t)
		exports = append(exports, "toll_allow")
	}
	if n := sanitizeCallerIDNumber(cidNum); n != "" {
		vars = append(vars, "outbound_caller_id_number="+n)
		exports = append(exports, "outbound_caller_id_number")
	}
	if len(vars) == 0 {
		return fmt.Sprintf("loopback/%s/%s", dest, context)
	}
	// Prepend loopback_export so the vars propagate to the dialplan leg (A→B).
	all := append([]string{"loopback_export=" + strings.Join(exports, ",")}, vars...)
	return fmt.Sprintf("[^^:%s]loopback/%s/%s", strings.Join(all, ":"), dest, context)
}

// AddToConference turns the controlled call into (or extends) a server-mixed conference and
// dials `destination` into it ("add people"). A bridge is strictly 2-party, so escalating a
// 1:1 to 3+ needs a mixer — mod_conference. On the first add it moves the call AND its bridged
// partner into a per-call ad-hoc room on the silent `softphone` conference profile; subsequent
// adds reuse the room. The new participant is dialed via a loopback through the call's own
// dialplan context (callInfo.AccountCode = the domain), so internal extensions and external
// numbers route exactly as a normal outbound call (caller ID / gateways via the dialplan).
//
// Merge correctness (verified against FreeSWITCH source):
//   - We do NOT use `uuid_transfer -both`: it resolves the partner from the SWITCH_BRIDGE_VARIABLE
//     channel var and silently degrades to moving only one leg when that var is empty/stale,
//     leaving the partner's bridge to end → hangup_after_bridge (default true) drops it. Instead
//     we resolve the partner explicitly, pre-arm park_after_bridge=true on both legs (park is
//     evaluated before hangup, so a torn-down leg is HELD not dropped), and transfer each leg.
//   - uuid_transfer is ASYNC, so we POLL conference_name until both legs have joined rather than
//     reading once (which races the join → false failure).
//   - Joining via the `conference:` named-bridge form sets CFLAG_BRIDGE_TO, which (together with
//     the silent profile) suppresses the "you are the only person" announcement / enter tones.
func (h *APIHandler) AddToConference(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	callUUID := vars["uuid"]

	if err := validateUUID(callUUID); err != nil {
		h.respondError(w, r, err.Error(), http.StatusBadRequest)
		return
	}

	callInfo, ok := h.validateCallContext(w, r, callUUID)
	if !ok {
		return
	}

	var req ConferenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, r, "Invalid request body", http.StatusBadRequest)
		return
	}

	dest := sanitizeDialDestination(req.Destination)
	if dest == "" {
		h.respondError(w, r, "destination is required", http.StatusBadRequest)
		return
	}

	// SP-L13: the loopback dial routes in the call's domain context (AccountCode). An empty
	// context would dial in the wrong/no context — refuse rather than dial blindly.
	if strings.TrimSpace(callInfo.AccountCode) == "" {
		h.respondError(w, r, "Call has no resolvable domain context", http.StatusBadGateway)
		return
	}

	// Outbound caller ID for the dialed party = the originating extension's outbound DID, read
	// from the controlling leg (a registered extension carries it as a directory var). The
	// loopback otherwise re-enters the dialplan WITHOUT it, so the FusionPBX outbound route's
	// `effective_caller_id_number=${outbound_caller_id_number}` resolves empty and the call goes
	// out as the conference placeholder 0000000000 — which carriers reject (503). Best-effort:
	// empty just falls back to prior behavior. Read before the transfers (var persists, but the
	// controlling leg is unambiguous here).
	outboundCid, _ := h.channelVar(callUUID, "outbound_caller_id_number")

	// Silent ad-hoc conference profile (FusionPBX DB profile "softphone": no enter/exit tones, no
	// "you are the only person" announcement, no MOH, no comfort noise) so a merge is seamless.
	const conferenceProfile = "softphone"
	room := "sp-" + callUUID

	// If the call is already in a conference, reuse that room; otherwise move the live bridge
	// into a new per-call room. A getvar error here is NOT "not conferenced" — fail fast (SP-L12).
	existing, err := h.channelVar(callUUID, "conference_name")
	if err != nil {
		h.respondError(w, r, fmt.Sprintf("Failed to read call state: %v", err), http.StatusBadGateway)
		return
	}
	if existing != "" {
		room = existing
	} else {
		// Resolve the bridge partner explicitly (see the -both caveat in the doc comment). An
		// empty partner is valid — a single, unbridged leg — not an error.
		partner, perr := h.channelVar(callUUID, "bridge_uuid")
		if perr != nil {
			h.respondError(w, r, fmt.Sprintf("Failed to read call state: %v", perr), http.StatusBadGateway)
			return
		}
		legs := []string{callUUID}
		if partner != "" {
			legs = append(legs, partner)
		}

		// Pre-arm hangup guards on every leg BEFORE transferring, so a leg whose bridge tears
		// down mid-merge is parked (held) instead of hung up (park is evaluated before hangup).
		for _, leg := range legs {
			h.eslClient.SendCommand(fmt.Sprintf("api uuid_setvar %s park_after_bridge true", leg))
			h.eslClient.SendCommand(fmt.Sprintf("api uuid_setvar %s hangup_after_bridge false", leg))
		}

		// Move each leg into the room explicitly via the `conference:` named-bridge form.
		for _, leg := range legs {
			if _, err := h.eslClient.SendCommand(fmt.Sprintf("api uuid_transfer %s conference:%s@%s inline", leg, room, conferenceProfile)); err != nil {
				h.respondError(w, r, fmt.Sprintf("Failed to start conference: %v", err), h.getErrorStatusCode(err))
				return
			}
		}

		// uuid_transfer is async (+OK = queued, not joined), so POLL until every original leg
		// reports conference_name == room. 503 only if the join never completes (e.g. the
		// profile is missing/disabled) — never from reading too early (SP-H7, race-free).
		if !h.waitForConferenceJoin(legs, room) {
			h.respondError(w, r,
				fmt.Sprintf("Conference join did not complete — the %q conference profile may be missing or disabled on this server", conferenceProfile),
				http.StatusServiceUnavailable)
			return
		}
	}

	// SP-H2: forward the originating extension's toll_allow (so the dialplan gates the loopback
	// dial like a normal outbound call) + its outbound caller-ID DID (so the carrier accepts it).
	dialString := buildConferenceDialString(req.TollAllow, outboundCid, callInfo.AccountCode, dest)
	if _, err := h.eslClient.SendCommand(fmt.Sprintf("api conference %s bgdial %s", room, dialString)); err != nil {
		h.respondError(w, r, fmt.Sprintf("Failed to add participant: %v", err), h.getErrorStatusCode(err))
		return
	}

	// SP-M5: bgdial is non-blocking (returns a Job-UUID immediately), so success here means
	// "queued", not "connected" — invalid/busy/no-answer surface later via conference events.
	// The message reflects that honestly rather than claiming the party is in the call.
	h.respondSuccess(w, r, fmt.Sprintf("Queued %s into conference %s", dest, room))
}
