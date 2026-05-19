package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
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

// BroadcastPayload matches the websocket worker /broadcast format.
type BroadcastPayload struct {
	UserUUID   string      `json:"userUuid,omitempty"`
	DomainUUID string      `json:"domainUuid,omitempty"`
	Topic      string      `json:"topic,omitempty"`
	Type       string      `json:"type"`
	Data       interface{} `json:"data"`
}

// InboundCallData is the payload broadcast when an inbound a-leg is detected.
// Field names use camelCase to match the WebSocket client's expected format.
type InboundCallData struct {
	CallUUID          string `json:"callUuid,omitempty"`
	CallerIDNumber    string `json:"callerIdNumber"`
	CallerIDName      string `json:"callerIdName,omitempty"`
	DestinationNumber string `json:"destinationNumber"`
	EventType         string `json:"event_type"`
	Timestamp         int64  `json:"timestamp"`
}

// EventSubscriber manages ESL event subscription and call event forwarding.
type EventSubscriber struct {
	eslHost     string
	eslPort     string
	eslPassword string

	broadcastURL    string
	broadcastSecret string

	// Inbound call notification config (optional, independent of originated call tracking)
	inboundWebhookURL   string // POST inbound call data to this URL
	inboundTopicPrefix  string // Broadcast topic prefix (e.g. "inbox:") — topic becomes "{prefix}{normalized_did}"

	mu       sync.RWMutex
	registry map[string]*CallRegistration // call_uuid -> registration

	inboundMu   sync.Mutex
	seenInbound map[string]time.Time // deduplication for inbound a-legs

	conn *eslgo.Conn
}

// EventSubscriberConfig holds configuration for the event subscriber.
type EventSubscriberConfig struct {
	ESLHost     string
	ESLPort     string
	ESLPassword string

	BroadcastURL    string
	BroadcastSecret string

	InboundWebhookURL  string
	InboundTopicPrefix string
}

func NewEventSubscriber(cfg EventSubscriberConfig) *EventSubscriber {
	return &EventSubscriber{
		eslHost:            cfg.ESLHost,
		eslPort:            cfg.ESLPort,
		eslPassword:        cfg.ESLPassword,
		broadcastURL:       cfg.BroadcastURL,
		broadcastSecret:    cfg.BroadcastSecret,
		inboundWebhookURL:  cfg.InboundWebhookURL,
		inboundTopicPrefix: cfg.InboundTopicPrefix,
		registry:           make(map[string]*CallRegistration),
		seenInbound:        make(map[string]time.Time),
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

	conn.RegisterEventListener(eslgo.EventListenAll, func(event *eslgo.Event) {
		es.handleEvent(event)
	})

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

	// Detect new inbound a-legs (external caller hitting a DID in the public context).
	// This fires before ring groups, queues, or IVRs process the call.
	if eventName == "CHANNEL_CREATE" {
		direction := event.Headers.Get("Call-Direction")
		callerContext := event.Headers.Get("Caller-Context")

		if direction == "inbound" && callerContext == "public" {
			es.handleInboundALeg(event, callUUID)
		}
	}

	// Originated call tracking — only forward events for calls we started via /calls/originate
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

	go es.broadcastEvent(reg, callEvent)

	if reg.CallbackURL != "" {
		go es.callWebhook(reg.CallbackURL, callEvent)
	}

	if eventName == "CHANNEL_DESTROY" {
		es.UnregisterCall(callUUID)
	}
}

// handleInboundALeg fires once per new inbound call. Deduplicates by call UUID
// so complex call flows (ring groups, queue retries, transfers) don't produce
// duplicate notifications for the same call.
func (es *EventSubscriber) handleInboundALeg(event *eslgo.Event, callUUID string) {
	if es.broadcastURL == "" && es.inboundWebhookURL == "" {
		return
	}

	es.inboundMu.Lock()
	if _, seen := es.seenInbound[callUUID]; seen {
		es.inboundMu.Unlock()
		return
	}
	es.seenInbound[callUUID] = time.Now()
	es.inboundMu.Unlock()

	callerIDNumber := eslDecode(event.Headers.Get("Caller-Caller-ID-Number"))
	callerIDName := eslDecode(event.Headers.Get("Caller-Caller-ID-Name"))
	destinationNumber := eslDecode(event.Headers.Get("Caller-Destination-Number"))
	domainUUID := eslDecode(event.Headers.Get("variable_domain_uuid"))
	if domainUUID == "" {
		domainUUID = eslDecode(event.Headers.Get("variable_dialed_domain_uuid"))
	}

	if callerIDNumber == "" || destinationNumber == "" {
		return
	}

	log.Printf("[Events] Inbound a-leg: %s → %s (call %s, domain %s)",
		callerIDNumber, destinationNumber, callUUID, domainUUID)

	data := InboundCallData{
		CallUUID:          callUUID,
		CallerIDNumber:    callerIDNumber,
		CallerIDName:      callerIDName,
		DestinationNumber: destinationNumber,
		EventType:         "call_ringing",
		Timestamp:         time.Now().Unix(),
	}

	if es.broadcastURL != "" {
		go es.broadcastInboundCall(domainUUID, destinationNumber, data)
	}

	if es.inboundWebhookURL != "" {
		go es.postInboundWebhook(data, domainUUID)
	}
}

// broadcastInboundCall sends the inbound call event directly to the WebSocket worker.
// If INBOUND_TOPIC_PREFIX is set (e.g. "inbox:"), the topic is "{prefix}{e164_number}",
// enabling per-number subscriptions. Otherwise broadcasts to the domain.
func (es *EventSubscriber) broadcastInboundCall(domainUUID, destinationNumber string, data InboundCallData) {
	payload := BroadcastPayload{
		DomainUUID: domainUUID,
		Type:       "incoming_call",
		Data:       data,
	}

	if es.inboundTopicPrefix != "" {
		normalized := normalizeToE164(destinationNumber)
		payload.Topic = es.inboundTopicPrefix + normalized
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[Events] Failed to marshal inbound broadcast: %v", err)
		return
	}

	req, err := http.NewRequest("POST", es.broadcastURL+"/broadcast", bytes.NewReader(body))
	if err != nil {
		log.Printf("[Events] Failed to create inbound broadcast request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if es.broadcastSecret != "" {
		req.Header.Set("X-Worker-Auth", es.broadcastSecret)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[Events] Inbound broadcast failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Printf("[Events] Inbound broadcast returned %d", resp.StatusCode)
	} else {
		log.Printf("[Events] Inbound broadcast sent for %s → %s", data.CallerIDNumber, payload.Topic)
	}
}

func (es *EventSubscriber) postInboundWebhook(data InboundCallData, domainUUID string) {
	payload := map[string]interface{}{
		"caller_id_number":   data.CallerIDNumber,
		"caller_id_name":     data.CallerIDName,
		"destination_number": data.DestinationNumber,
		"domain_uuid":        domainUUID,
		"call_uuid":          data.CallUUID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	req, err := http.NewRequest("POST", es.inboundWebhookURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[Events] Inbound webhook failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Printf("[Events] Inbound webhook returned %d for call %s", resp.StatusCode, data.CallUUID)
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

// cleanupStaleRegistrations removes entries older than maxAge to prevent memory leaks.
func (es *EventSubscriber) cleanupStaleRegistrations(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)

	es.mu.Lock()
	for uuid, reg := range es.registry {
		if reg.CreatedAt.Before(cutoff) {
			log.Printf("[Events] Cleaning up stale registration for call %s", uuid)
			delete(es.registry, uuid)
		}
	}
	es.mu.Unlock()

	es.inboundMu.Lock()
	for uuid, seen := range es.seenInbound {
		if seen.Before(cutoff) {
			delete(es.seenInbound, uuid)
		}
	}
	es.inboundMu.Unlock()
}

// eslDecode URL-decodes ESL header values. FreeSWITCH ESL encodes special
// characters (e.g. + becomes %2B) in header values.
func eslDecode(s string) string {
	decoded, err := url.QueryUnescape(s)
	if err != nil {
		return s
	}
	return decoded
}

// normalizeToE164 converts a phone number to E.164 format for topic matching.
// Handles North American numbers (10 digits → +1xxx, 11 digits starting with 1 → +1xxx).
// Numbers already starting with + are returned as-is after stripping non-digits.
func normalizeToE164(number string) string {
	if number == "" {
		return number
	}

	hasPlus := strings.HasPrefix(number, "+")

	var digits strings.Builder
	for _, c := range number {
		if c >= '0' && c <= '9' {
			digits.WriteRune(c)
		}
	}
	d := digits.String()

	if hasPlus && len(d) >= 10 {
		return "+" + d
	}
	if len(d) == 10 {
		return "+1" + d
	}
	if len(d) == 11 && strings.HasPrefix(d, "1") {
		return "+" + d
	}
	return number
}
