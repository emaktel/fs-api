package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/percipia/eslgo"
	"github.com/percipia/eslgo/command"
)

const Version = "0.1.0"

// Configuration with sane defaults
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

var (
	FSAPI_PORT   = getEnv("FSAPI_PORT", "37274")
	ESL_HOST     = getEnv("ESL_HOST", "localhost")
	ESL_PORT     = getEnv("ESL_PORT", "8021")
	ESL_PASSWORD = getEnv("ESL_PASSWORD", "ClueCon")
)

// UUID Validation
func validateUUID(uuidStr string) error {
	if _, err := uuid.Parse(uuidStr); err != nil {
		return fmt.Errorf("invalid UUID format: %s", uuidStr)
	}
	return nil
}

// Path Validation for recording filenames
func validateFilePath(path string) error {
	if path == "" {
		return fmt.Errorf("path cannot be empty")
	}

	cleanPath := filepath.Clean(path)

	// Must be absolute path
	if !filepath.IsAbs(cleanPath) {
		return fmt.Errorf("path must be absolute")
	}

	// Check for path traversal attempts
	if strings.Contains(path, "..") {
		return fmt.Errorf("path traversal not allowed")
	}

	return nil
}

// Structured logging helper
type LogEntry struct {
	Timestamp string
	RequestID string
	Level     string
	Message   string
	Error     string
}

func logInfo(requestID, message string) {
	log.Printf("[INFO] [%s] %s", requestID, message)
}

func logError(requestID, message string, err error) {
	if err != nil {
		log.Printf("[ERROR] [%s] %s: %v", requestID, message, err)
	} else {
		log.Printf("[ERROR] [%s] %s", requestID, message)
	}
}

func logWarn(requestID, message string) {
	log.Printf("[WARN] [%s] %s", requestID, message)
}

// Context keys
type contextKey string

const requestIDKey contextKey = "requestID"

func getRequestID(r *http.Request) string {
	if reqID, ok := r.Context().Value(requestIDKey).(string); ok {
		return reqID
	}
	return "unknown"
}

// Request/Response Structures
type SuccessResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type ErrorResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type HangupRequest struct {
	Cause string `json:"cause"`
}

type TransferRequest struct {
	Destination string `json:"destination"`
	Context     string `json:"context"`
}

type BridgeRequest struct {
	UUIDA string `json:"uuid_a"`
	UUIDB string `json:"uuid_b"`
}

type HoldRequest struct {
	Action string `json:"action"`
}

type RecordRequest struct {
	Action   string `json:"action"`
	Filename string `json:"filename,omitempty"`
}

type DTMFRequest struct {
	Digits   string `json:"digits"`
	Duration int    `json:"duration,omitempty"`
}

type OriginateRequest struct {
	ALeg             string                 `json:"aleg"`
	BLeg             string                 `json:"bleg"`
	Dialplan         string                 `json:"dialplan,omitempty"`
	Context          string                 `json:"context,omitempty"`
	CallerIDName     string                 `json:"caller_id_name,omitempty"`
	CallerIDNumber   string                 `json:"caller_id_number,omitempty"`
	TimeoutSec       int                    `json:"timeout_sec,omitempty"`
	ChannelVariables map[string]interface{} `json:"channel_variables,omitempty"`
}

// ESL Client Interface
type ESLClient interface {
	SendCommand(cmd string) (string, error)
	Close() error
}

// ESLgo implementation with connection pooling
type ESLgoClient struct {
	host     string
	port     string
	password string
	mu       sync.Mutex
	conn     *eslgo.Conn
}

func NewESLClient(host, port, password string) ESLClient {
	return &ESLgoClient{
		host:     host,
		port:     port,
		password: password,
	}
}

func (esl *ESLgoClient) getConnection() (*eslgo.Conn, error) {
	esl.mu.Lock()
	defer esl.mu.Unlock()

	// If connection exists and is alive, reuse it
	if esl.conn != nil {
		return esl.conn, nil
	}

	// Create new connection
	conn, err := eslgo.Dial(esl.host+":"+esl.port, esl.password, func() {
		log.Println("ESL connection disconnected")
		esl.mu.Lock()
		esl.conn = nil
		esl.mu.Unlock()
	})
	if err != nil {
		log.Printf("Failed to connect to ESL: %v", err)
		return nil, fmt.Errorf("ESL connection failed: %v", err)
	}

	esl.conn = conn
	log.Println("New ESL connection established")
	return conn, nil
}

func (esl *ESLgoClient) SendCommand(cmd string) (string, error) {
	log.Printf("ESL Command: %s", cmd)

	// Get or create connection
	conn, err := esl.getConnection()
	if err != nil {
		return "", err
	}

	// Parse the command string into command and arguments
	// Expected format: "api <command> <arguments>"
	parts := strings.SplitN(cmd, " ", 3)
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid command format: %s", cmd)
	}

	// Skip the "api" prefix and extract command and arguments
	var apiCmd command.API
	if parts[0] == "api" {
		apiCmd.Command = parts[1]
		if len(parts) > 2 {
			apiCmd.Arguments = parts[2]
		}
	} else {
		return "", fmt.Errorf("unsupported command type: %s", parts[0])
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Send the command and get response
	response, err := conn.SendCommand(ctx, apiCmd)
	if err != nil {
		log.Printf("Failed to send ESL command: %v", err)
		// Connection might be broken, clear it
		esl.mu.Lock()
		if esl.conn != nil {
			esl.conn.Close()
			esl.conn = nil
		}
		esl.mu.Unlock()
		return "", fmt.Errorf("ESL command failed: %v", err)
	}

	// Get the response body
	responseText := response.GetHeader("Reply-Text")
	responseBody := string(response.Body)

	log.Printf("ESL Response: %s", responseText)

	// Check if command was successful
	if strings.HasPrefix(responseText, "-ERR") {
		return responseText, fmt.Errorf("ESL error: %s", responseText)
	}

	// For commands like 'status', the data is in the body, not Reply-Text
	if responseBody != "" {
		return responseBody, nil
	}

	return responseText, nil
}

func (esl *ESLgoClient) Close() error {
	esl.mu.Lock()
	defer esl.mu.Unlock()

	if esl.conn != nil {
		esl.conn.Close()
		esl.conn = nil
	}
	return nil
}

// API Handlers
type APIHandler struct {
	eslClient ESLClient
}

func NewAPIHandler() *APIHandler {
	return &APIHandler{
		eslClient: NewESLClient(ESL_HOST, ESL_PORT, ESL_PASSWORD),
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

	var req TransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, r, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Destination == "" || req.Context == "" {
		h.respondError(w, r, "destination and context are required", http.StatusBadRequest)
		return
	}

	cmd := fmt.Sprintf("api uuid_transfer %s %s XML %s", callUUID, req.Destination, req.Context)
	_, err := h.eslClient.SendCommand(cmd)
	if err != nil {
		statusCode := h.getErrorStatusCode(err)
		h.respondError(w, r, fmt.Sprintf("Failed to transfer call: %v", err), statusCode)
		return
	}

	h.respondSuccess(w, r, fmt.Sprintf("Call %s transferred to %s in context %s", callUUID, req.Destination, req.Context))
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
	if req.BLeg == "" {
		h.respondError(w, r, "bleg is required", http.StatusBadRequest)
		return
	}

	// Build channel variables string
	var channelVars string
	if len(req.ChannelVariables) > 0 {
		vars := []string{}
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
		channelVars = fmt.Sprintf("{%s}", strings.Join(vars, ","))
	}

	// Build the originate command
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

	// Add optional parameters
	if req.Dialplan != "" {
		cmd.WriteString(" ")
		cmd.WriteString(req.Dialplan)
	}

	if req.Context != "" {
		cmd.WriteString(" ")
		cmd.WriteString(req.Context)
	}

	if req.CallerIDName != "" {
		cmd.WriteString(" ")
		cmd.WriteString(req.CallerIDName)
	}

	if req.CallerIDNumber != "" {
		cmd.WriteString(" ")
		cmd.WriteString(req.CallerIDNumber)
	}

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

// Middleware to limit request body size
func requestSizeLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1048576) // 1MB limit
		next.ServeHTTP(w, r)
	})
}

// Middleware to add request ID to context
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := uuid.New().String()
		ctx := context.WithValue(r.Context(), requestIDKey, requestID)
		w.Header().Set("X-Request-ID", requestID)
		logInfo(requestID, fmt.Sprintf("%s %s", r.Method, r.URL.Path))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func main() {
	handler := NewAPIHandler()

	r := mux.NewRouter()

	// Apply middlewares
	r.Use(requestIDMiddleware)
	r.Use(requestSizeLimitMiddleware)

	v1 := r.PathPrefix("/v1").Subrouter()

	// Register all endpoints
	v1.HandleFunc("/calls/{uuid}/hangup", handler.HangupCall).Methods("POST")
	v1.HandleFunc("/calls/{uuid}/transfer", handler.TransferCall).Methods("POST")
	v1.HandleFunc("/calls/bridge", handler.BridgeCalls).Methods("POST")
	v1.HandleFunc("/calls/{uuid}/answer", handler.AnswerCall).Methods("POST")
	v1.HandleFunc("/calls/{uuid}/hold", handler.ControlHold).Methods("POST")
	v1.HandleFunc("/calls/{uuid}/record", handler.ControlRecording).Methods("POST")
	v1.HandleFunc("/calls/{uuid}/dtmf", handler.SendDTMF).Methods("POST")
	v1.HandleFunc("/calls/{uuid}/park", handler.ParkCall).Methods("POST")
	v1.HandleFunc("/calls/originate", handler.OriginateCall).Methods("POST")
	v1.HandleFunc("/status", handler.GetStatus).Methods("GET")

	// Improved health check endpoint that tests ESL connection
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Try to send a simple command to test ESL connection
		_, err := handler.eslClient.SendCommand("api status")
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
	}).Methods("GET")

	// Bind to all interfaces (0.0.0.0) instead of just localhost
	addr := fmt.Sprintf(":%s", FSAPI_PORT)
	log.Printf("FreeSWITCH Call Control API v%s starting on %s (all interfaces)", Version, addr)
	log.Printf("ESL configured for %s:%s", ESL_HOST, ESL_PORT)

	// Configure HTTP server with timeouts
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("Server configured with ReadTimeout: 15s, WriteTimeout: 15s, IdleTimeout: 60s")

	// Start server in a goroutine
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	log.Println("Server started successfully")

	// Wait for interrupt signal to gracefully shutdown the server
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	// Create shutdown context with 30 second timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Attempt graceful shutdown
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	} else {
		log.Println("Server shutdown gracefully")
	}

	// Close ESL connection
	if err := handler.eslClient.Close(); err != nil {
		log.Printf("Error closing ESL client: %v", err)
	}

	log.Println("Server exited")
}
