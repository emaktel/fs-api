package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// Configuration with sane defaults
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

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
