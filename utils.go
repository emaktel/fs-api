package main

import (
	"fmt"
	"log"
	"os"
)

// Configuration with sane defaults
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// validateUUID accepts only the canonical lowercase form FreeSWITCH gives
// call uuids, so a call id can't carry braces, a urn: prefix or spaces into a
// command.
func validateUUID(uuidStr string) error {
	if !canonicalUUID.MatchString(uuidStr) {
		return fmt.Errorf("invalid UUID format: %q", uuidStr)
	}
	return nil
}

// Structured logging helpers
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
