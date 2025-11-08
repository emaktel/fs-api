package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/percipia/eslgo"
	"github.com/percipia/eslgo/command"
)

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
