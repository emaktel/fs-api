package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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
