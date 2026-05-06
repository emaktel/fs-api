package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/percipia/eslgo"
	"github.com/percipia/eslgo/command"
)

// CallRegistration tracks an originated call and where to send its events.
type CallRegistration struct {
	CallUUID    string `json:"call_uuid"`
	UserUUID    string `json:"user_uuid"`
	DomainUUID  string `json:"domain_uuid"`
	DomainName  string `json:"domain_name"`
	CallbackURL string `json:"callback_url,omitempty"`
	CreatedAt   time.Time
}

// CallEvent is the payload forwarded to the WebSocket worker and/or callback URL.
type CallEvent struct {
	CallUUID          string `json:"call_uuid"`
	Event             string `json:"event"`
	Direction         string `json:"direction,omitempty"`
	CallerIDNumber    string `json:"caller_id_number,omitempty"`
	CallerIDName      string `json:"caller_id_name,omitempty"`
	DestinationNumber string `json:"destination_number,omitempty"`
	Context           string `json:"context,omitempty"`
	CallState         string `json:"callstate,omitempty"`
	HangupCause       string `json:"hangup_cause,omitempty"`
	Timestamp         int64  `json:"timestamp"`
}

// BroadcastPayload matches the fusion-websocket-worker /broadcast format.
type BroadcastPayload struct {
	UserUUID   string    `json:"userUuid"`
	DomainUUID string    `json:"domainUuid"`
	Topic      string    `json:"topic"`
	Type       string    `json:"type"`
	Data       CallEvent `json:"data"`
}

// EventSubscriber manages ESL event subscription and call event forwarding.
type EventSubscriber struct {
	eslHost     string
	eslPort     string
	eslPassword string

	broadcastURL    string
	broadcastSecret string

	mu       sync.RWMutex
	registry map[string]*CallRegistration // call_uuid -> registration

	conn *eslgo.Conn
}

func NewEventSubscriber(eslHost, eslPort, eslPassword, broadcastURL, broadcastSecret string) *EventSubscriber {
	return &EventSubscriber{
		eslHost:         eslHost,
		eslPort:         eslPort,
		eslPassword:     eslPassword,
		broadcastURL:    broadcastURL,
		broadcastSecret: broadcastSecret,
		registry:        make(map[string]*CallRegistration),
	}
}

// RegisterCall adds a call to the registry so its events will be forwarded.
func (es *EventSubscriber) RegisterCall(reg *CallRegistration) {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.registry[reg.CallUUID] = reg
	log.Printf("[Events] Registered call %s for user %s on %s", reg.CallUUID, reg.UserUUID, reg.DomainName)
}

// UnregisterCall removes a call from the registry.
func (es *EventSubscriber) UnregisterCall(callUUID string) {
	es.mu.Lock()
	defer es.mu.Unlock()
	delete(es.registry, callUUID)
}

// getRegistration returns the registration for a call UUID, or nil.
func (es *EventSubscriber) getRegistration(callUUID string) *CallRegistration {
	es.mu.RLock()
	defer es.mu.RUnlock()
	return es.registry[callUUID]
}

// Start connects to ESL and begins listening for events. Blocks until ctx is cancelled.
func (es *EventSubscriber) Start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := es.connect(ctx)
		if err != nil {
			log.Printf("[Events] ESL connection error: %v, reconnecting in 5s", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (es *EventSubscriber) connect(ctx context.Context) error {
	log.Println("[Events] Connecting to ESL for event subscription...")

	disconnected := make(chan struct{})
	conn, err := eslgo.Dial(es.eslHost+":"+es.eslPort, es.eslPassword, func() {
		log.Println("[Events] ESL event connection disconnected")
		close(disconnected)
	})
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}
	es.conn = conn
	log.Println("[Events] ESL event connection established")

	// Subscribe only to the event types we care about
	subCtx, subCancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = conn.SendCommand(subCtx, command.Event{
		Format: "plain",
		Listen: []string{
			"CHANNEL_CREATE",
			"CHANNEL_ANSWER",
			"CHANNEL_HANGUP",
			"CHANNEL_BRIDGE",
			"CHANNEL_DESTROY",
		},
	})
	subCancel()
	if err != nil {
		conn.ExitAndClose()
		return fmt.Errorf("event subscription failed: %w", err)
	}
	log.Println("[Events] Subscribed to CHANNEL_CREATE, CHANNEL_ANSWER, CHANNEL_HANGUP, CHANNEL_BRIDGE, CHANNEL_DESTROY")

	// Register event listener
	conn.RegisterEventListener(eslgo.EventListenAll, func(event *eslgo.Event) {
		es.handleEvent(event)
	})

	// Wait for disconnect or context cancellation
	select {
	case <-ctx.Done():
		conn.ExitAndClose()
		return nil
	case <-disconnected:
		return fmt.Errorf("connection lost")
	}
}

func (es *EventSubscriber) handleEvent(event *eslgo.Event) {
	eventName := event.Headers.Get("Event-Name")
	callUUID := event.Headers.Get("Unique-ID")

	if callUUID == "" || eventName == "" {
		return
	}

	// Only process events for calls we originated
	reg := es.getRegistration(callUUID)
	if reg == nil {
		return
	}

	callEvent := CallEvent{
		CallUUID:          callUUID,
		Event:             eventName,
		Direction:         event.Headers.Get("Call-Direction"),
		CallerIDNumber:    event.Headers.Get("Caller-Caller-ID-Number"),
		CallerIDName:      event.Headers.Get("Caller-Caller-ID-Name"),
		DestinationNumber: event.Headers.Get("Caller-Destination-Number"),
		Context:           event.Headers.Get("Caller-Context"),
		CallState:         event.Headers.Get("Channel-Call-State"),
		Timestamp:         time.Now().Unix(),
	}

	if eventName == "CHANNEL_HANGUP" || eventName == "CHANNEL_DESTROY" {
		callEvent.HangupCause = event.Headers.Get("Hangup-Cause")
	}

	log.Printf("[Events] %s for call %s (user %s)", eventName, callUUID, reg.UserUUID)

	// Forward to WebSocket worker
	go es.broadcastEvent(reg, callEvent)

	// Forward to callback URL if registered
	if reg.CallbackURL != "" {
		go es.callWebhook(reg.CallbackURL, callEvent)
	}

	// Clean up on call destroy
	if eventName == "CHANNEL_DESTROY" {
		es.UnregisterCall(callUUID)
	}
}

func (es *EventSubscriber) broadcastEvent(reg *CallRegistration, event CallEvent) {
	if es.broadcastURL == "" {
		return
	}

	payload := BroadcastPayload{
		UserUUID:   reg.UserUUID,
		DomainUUID: reg.DomainUUID,
		Topic:      "calls",
		Type:       "call_event",
		Data:       event,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[Events] Failed to marshal broadcast: %v", err)
		return
	}

	req, err := http.NewRequest("POST", es.broadcastURL+"/broadcast", bytes.NewReader(body))
	if err != nil {
		log.Printf("[Events] Failed to create broadcast request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if es.broadcastSecret != "" {
		req.Header.Set("X-Worker-Auth", es.broadcastSecret)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[Events] Broadcast failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Printf("[Events] Broadcast returned %d", resp.StatusCode)
	}
}

func (es *EventSubscriber) callWebhook(callbackURL string, event CallEvent) {
	body, err := json.Marshal(event)
	if err != nil {
		return
	}

	req, err := http.NewRequest("POST", callbackURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[Events] Webhook to %s failed: %v", callbackURL, err)
		return
	}
	defer resp.Body.Close()
}

// cleanupStaleRegistrations removes registrations older than maxAge.
// Called periodically to prevent memory leaks from calls that never got CHANNEL_DESTROY.
func (es *EventSubscriber) cleanupStaleRegistrations(maxAge time.Duration) {
	es.mu.Lock()
	defer es.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	for uuid, reg := range es.registry {
		if reg.CreatedAt.Before(cutoff) {
			log.Printf("[Events] Cleaning up stale registration for call %s", uuid)
			delete(es.registry, uuid)
		}
	}
}
