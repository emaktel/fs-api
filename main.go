package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/mux"
)

const Version = "0.3.0"

var (
	FSAPI_PORT        = getEnv("FSAPI_PORT", "37274")
	ESL_HOST          = getEnv("ESL_HOST", "localhost")
	ESL_PORT          = getEnv("ESL_PORT", "8021")
	ESL_PASSWORD      = getEnv("ESL_PASSWORD", "ClueCon")
	FSAPI_AUTH_TOKENS = getEnv("FSAPI_AUTH_TOKENS", "")
)

func main() {
	handler := NewAPIHandler(ESL_HOST, ESL_PORT, ESL_PASSWORD)

	// Parse authentication tokens
	var authTokens []string
	if FSAPI_AUTH_TOKENS != "" {
		tokens := strings.Split(FSAPI_AUTH_TOKENS, ",")
		for _, token := range tokens {
			trimmed := strings.TrimSpace(token)
			if trimmed != "" {
				authTokens = append(authTokens, trimmed)
			}
		}
	}

	r := mux.NewRouter()

	// Apply middlewares (auth must be first)
	r.Use(requestIDMiddleware)
	r.Use(bearerAuthMiddleware(authTokens))
	r.Use(contextAuthMiddleware)
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
	v1.HandleFunc("/calls", handler.ListCalls).Methods("GET")
	v1.HandleFunc("/calls/{uuid}", handler.GetCallDetails).Methods("GET")
	v1.HandleFunc("/status", handler.GetStatus).Methods("GET")

	// Health check endpoint
	r.HandleFunc("/health", handler.HealthCheck).Methods("GET")

	// Bind to all interfaces (0.0.0.0) instead of just localhost
	addr := fmt.Sprintf(":%s", FSAPI_PORT)
	log.Printf("FreeSWITCH Call Control API v%s starting on %s (all interfaces)", Version, addr)
	log.Printf("ESL configured for %s:%s", ESL_HOST, ESL_PORT)

	// Log authentication status
	if len(authTokens) > 0 {
		log.Printf("Bearer token authentication: ENABLED (%d token(s) configured)", len(authTokens))
		log.Printf("Note: Localhost requests bypass authentication")
	} else {
		log.Printf("Bearer token authentication: DISABLED (no tokens configured)")
		log.Printf("WARNING: API is accessible without authentication from remote hosts")
	}

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
