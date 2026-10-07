package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"sync"
	"time"

	"github.com/percipia/eslgo"
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
	UserUUIDs  []string    `json:"userUuids,omitempty"`
	DomainUUID string      `json:"domainUuid,omitempty"`
	Topic      string      `json:"topic,omitempty"`
	Type       string      `json:"type"`
	Data       interface{} `json:"data"`
}

// CallStateData is broadcast when a tracked call's state changes (answered, hangup, etc.).
type CallStateData struct {
	CallUUID    string `json:"callUuid"`
	State       string `json:"state"`
	HangupCause string `json:"hangupCause,omitempty"`
	Extension   string `json:"extension,omitempty"`
	Timestamp   int64  `json:"timestamp"`
}

// RingCallData is the payload broadcast when a specific extension starts ringing.
type RingCallData struct {
	CallUUID          string `json:"callUuid,omitempty"`
	CallerIDNumber    string `json:"callerIdNumber"`
	CallerIDName      string `json:"callerIdName,omitempty"`
	Extension         string `json:"extension"`
	Domain            string `json:"domain"`
	DestinationNumber string `json:"destinationNumber"`
	Timestamp         int64  `json:"timestamp"`
}

// ringRegistration tracks a b-leg so answer/hangup events can be forwarded to the users.
type ringRegistration struct {
	tenants   []tenantRecipients
	callerID  string
	extension string
	createdAt time.Time
}

// tenantRecipients is one broadcast's audience: users who share the tenant
// (domain_uuid) that resolve_extension_user returned for them, which is the
// extension's tenant.
type tenantRecipients struct {
	domainUuid string
	userUuids  []string
}

// groupByTenant splits resolved users into one audience per tenant, in the
// order each tenant first appears. resolve_extension_user filters on a single
// domain_name, so this is normally one group; domain_name uniqueness is not a
// DB constraint, and the worker requires every named broadcast to carry the
// recipients' tenant, so a mixed result is split rather than stamped with one
// user's tenant.
func groupByTenant(users []resolvedUser) []tenantRecipients {
	var groups []tenantRecipients
	index := make(map[string]int)
	for _, u := range users {
		i, ok := index[u.domainUuid]
		if !ok {
			i = len(groups)
			index[u.domainUuid] = i
			groups = append(groups, tenantRecipients{domainUuid: u.domainUuid})
		}
		groups[i].userUuids = append(groups[i].userUuids, u.userUuid)
	}
	return groups
}

// resolvedUser holds a single user resolved from an extension.
type resolvedUser struct {
	userUuid   string
	domainUuid string
}

// userCacheEntry holds resolved extension → users mapping with TTL.
type userCacheEntry struct {
	users      []resolvedUser
	resolvedAt time.Time
}

// EventSubscriber manages ESL event subscription and call event forwarding.
type EventSubscriber struct {
	eslHost     string
	eslPort     string
	eslPassword string

	broadcastURL    string
	broadcastSecret string

	// User resolution config — resolves extension+domain → user for targeted call pops
	resolveURL    string // REST endpoint that returns [{user_uuid, domain_uuid}] given extension + domain
	resolveSecret string // Optional auth token for the resolve endpoint

	mu       sync.RWMutex
	registry map[string]*CallRegistration // call_uuid -> registration

	userCacheMu sync.RWMutex
	userCache   map[string]*userCacheEntry // "domain:extension" -> resolved user

	// ringRegistry tracks b-leg call UUIDs so we can forward answer/hangup events
	// to the correct user. Populated on successful ring broadcast, cleaned up on CHANNEL_DESTROY.
	ringMu       sync.RWMutex
	ringRegistry map[string]*ringRegistration // b-leg call_uuid -> user target

	// seenRing deduplicates ring broadcasts so each user gets at most one pop per
	// inbound call, even when FreeSWITCH creates multiple b-legs for the same
	// extension (ring groups, retries, simultaneous ring).
	seenRingMu sync.Mutex
	seenRing   map[string]time.Time // "aleg_uuid:user_uuid" -> first seen

	// eventProbe paces the liveness probe on the event connection, and
	// reconnectDelay is the pause before Start redials it.
	eventProbe     probeTiming
	reconnectDelay time.Duration

	// ctx is the lifecycle context captured in Start; broadcast retry backoff
	// sleeps observe it so they unblock immediately on shutdown.
	ctx context.Context

	// httpClient is shared by all worker broadcasts (5s timeout per attempt).
	httpClient *http.Client
	// retryBaseDelay is the broadcast backoff base (a field so tests can shrink it).
	retryBaseDelay time.Duration
}

// EventSubscriberConfig holds configuration for the event subscriber.
type EventSubscriberConfig struct {
	ESLHost     string
	ESLPort     string
	ESLPassword string

	BroadcastURL    string
	BroadcastSecret string

	ResolveURL    string
	ResolveSecret string
}

func NewEventSubscriber(cfg EventSubscriberConfig) *EventSubscriber {
	return &EventSubscriber{
		eslHost:         cfg.ESLHost,
		eslPort:         cfg.ESLPort,
		eslPassword:     cfg.ESLPassword,
		broadcastURL:    cfg.BroadcastURL,
		broadcastSecret: cfg.BroadcastSecret,
		resolveURL:      cfg.ResolveURL,
		resolveSecret:   cfg.ResolveSecret,
		registry:        make(map[string]*CallRegistration),
		userCache:       make(map[string]*userCacheEntry),
		ringRegistry:    make(map[string]*ringRegistration),
		seenRing:        make(map[string]time.Time),
		eventProbe:      defaultEventProbe,
		reconnectDelay:  5 * time.Second,
		httpClient:      &http.Client{Timeout: 5 * time.Second},
		retryBaseDelay:  defaultBroadcastRetryBaseDelay,
	}
}

// probeTiming: probe the event connection every interval; a connection with
// no traffic for interval+timeout is dead.
type probeTiming struct {
	interval time.Duration
	timeout  time.Duration
}

var defaultEventProbe = probeTiming{interval: 15 * time.Second, timeout: 5 * time.Second}

// lifecycleCtx is the context captured in Start (Background before Start).
func (es *EventSubscriber) lifecycleCtx() context.Context {
	if es.ctx == nil {
		return context.Background()
	}
	return es.ctx
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
	es.ctx = ctx
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := es.connect(ctx)
		if err != nil {
			log.Printf("[Events] ESL connection error: %v, reconnecting in %s", err, es.reconnectDelay)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(es.reconnectDelay):
		}
	}
}

// eventSubscription is the `event plain` command sent on the event connection.
const eventSubscription = "event plain CHANNEL_CREATE CHANNEL_ANSWER CHANNEL_HANGUP CHANNEL_BRIDGE CHANNEL_DESTROY"

// connect runs one event connection until it ends, returning nil only on shutdown.
//
// The connection is read directly rather than through eslgo: eslgo reports a
// lost connection only when FreeSWITCH sends a disconnect-notice, so one that
// ended without it (EOF, a FreeSWITCH restart, a dead peer) left the
// subscriber waiting forever with events silently stopped. Here any read
// error ends the connection, and an `api status` probe every
// eventProbe.interval guarantees traffic, so a connection that stays silent
// for interval+timeout is treated as dead. Start then redials.
func (es *EventSubscriber) connect(ctx context.Context) error {
	log.Println("[Events] Connecting to ESL for event subscription...")

	setupCtx, setupCancel := context.WithTimeout(ctx, 10*time.Second)
	defer setupCancel()
	deadline, _ := setupCtx.Deadline()
	conn, err := dialESL(setupCtx, net.JoinHostPort(es.eslHost, es.eslPort), es.eslPassword, deadline)
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}
	defer conn.Close()
	// Shutdown unblocks the read loop below.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	log.Println("[Events] ESL event connection established")

	if err := eslCommand(conn, eventSubscription); err != nil {
		return fmt.Errorf("event subscription failed: %w", err)
	}
	setupCancel()
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear deadline: %w", err)
	}
	log.Println("[Events] Subscribed to CHANNEL_CREATE, CHANNEL_ANSWER, CHANNEL_HANGUP, CHANNEL_BRIDGE, CHANNEL_DESTROY")

	probeCtx, probeCancel := context.WithCancel(ctx)
	defer probeCancel()
	go es.probeEventConn(probeCtx, conn)

	idle := es.eventProbe.interval + es.eventProbe.timeout
	for {
		if err := conn.SetReadDeadline(time.Now().Add(idle)); err != nil {
			return fmt.Errorf("set read deadline: %w", err)
		}
		hdr, body, err := readESLFrame(conn.rd)
		if errors.Is(err, errOversizedFrame) {
			// The body was read and discarded; the stream is still in sync.
			log.Printf("[Events] Skipped an oversized ESL frame (%s): %v", hdr.Get("Content-Type"), err)
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("event connection lost: %w", err)
		}
		switch ct := hdr.Get("Content-Type"); ct {
		case "text/event-plain":
			event, err := parsePlainEvent(body)
			if err != nil {
				log.Printf("[Events] Unparseable ESL event: %v", err)
				continue
			}
			go es.handleEvent(event)
		case "text/disconnect-notice":
			return fmt.Errorf("FreeSWITCH sent a disconnect-notice")
		case "api/response":
			// A probe reply; receiving it already renewed the read deadline.
		default:
			log.Printf("[Events] Ignoring unexpected ESL frame %q on the event connection", ct)
		}
	}
}

// probeEventConn writes `api status` every eventProbe.interval. The replies
// are consumed by connect's read loop; a write that fails closes the
// connection so the read loop ends.
func (es *EventSubscriber) probeEventConn(ctx context.Context, conn net.Conn) {
	ticker := time.NewTicker(es.eventProbe.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := conn.SetWriteDeadline(time.Now().Add(es.eventProbe.timeout))
			if err == nil {
				_, err = io.WriteString(conn, "api status\n\n")
			}
			if err != nil {
				log.Printf("[Events] ESL event connection probe failed: %v; closing it to reconnect", err)
				conn.Close()
				return
			}
		}
	}
}

// parsePlainEvent decodes a text/event-plain body: MIME-style headers whose
// values stay URL-encoded (handleEvent decodes the ones it uses), as eslgo did.
func parsePlainEvent(body []byte) (*eslgo.Event, error) {
	headers, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(body))).ReadMIMEHeader()
	if err != nil && !(errors.Is(err, io.EOF) && len(headers) > 0) {
		return nil, err
	}
	return &eslgo.Event{Headers: headers}, nil
}

func (es *EventSubscriber) handleEvent(event *eslgo.Event) {
	eventName := event.Headers.Get("Event-Name")
	callUUID := event.Headers.Get("Unique-ID")

	if callUUID == "" || eventName == "" {
		return
	}

	if eventName == "CHANNEL_CREATE" {
		direction := event.Headers.Get("Call-Direction")
		callerContext := event.Headers.Get("Caller-Context")

		// Detect b-legs ringing extensions (FreeSWITCH calling out to a user's phone).
		// direction=outbound in a domain context means an extension is being rung.
		if direction == "outbound" && callerContext != "public" && callerContext != "" {
			es.handleExtensionRing(event, callUUID, callerContext)
		}
	}

	// Forward answer/hangup events for tracked b-legs (extension ringing lifecycle).
	// CHANNEL_DESTROY is skipped — CHANNEL_HANGUP always fires first with the hangup cause.
	if eventName == "CHANNEL_ANSWER" || eventName == "CHANNEL_HANGUP" {
		es.handleRingLifecycle(callUUID, eventName, event)
	}

	// Clean up ring registry on destroy (no broadcast needed)
	if eventName == "CHANNEL_DESTROY" {
		es.ringMu.Lock()
		delete(es.ringRegistry, callUUID)
		es.ringMu.Unlock()
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

const broadcastMaxRetries = 3

// defaultBroadcastRetryBaseDelay is the base for exponential backoff (500ms, 1s, 2s).
const defaultBroadcastRetryBaseDelay = 500 * time.Millisecond

// sendBroadcast POSTs a payload to the WebSocket worker's /broadcast endpoint with
// bounded exponential-backoff retry (3 attempts: 500ms, 1s, 2s). Network errors and
// 5xx responses are retried; a 4xx is a permanent client error (bad/unauthorized
// payload) and returns immediately without retrying. Returns nil on a 2xx response.
// The label identifies the event type in retry logs.
//
// This is the single delivery path for every worker broadcast, so a momentary worker
// outage (e.g. a deploy rollover) no longer permanently drops call events.
func (es *EventSubscriber) sendBroadcast(label string, payload BroadcastPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", label, err)
	}

	ctx := es.lifecycleCtx()

	for attempt := 1; attempt <= broadcastMaxRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		req, err := http.NewRequestWithContext(ctx, "POST", es.broadcastURL+"/broadcast", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create %s request: %w", label, err)
		}
		req.Header.Set("Content-Type", "application/json")
		if es.broadcastSecret != "" {
			req.Header.Set("X-Worker-Auth", es.broadcastSecret)
		}

		resp, err := es.httpClient.Do(req)
		if err != nil {
			log.Printf("[Events] %s broadcast attempt %d/%d failed: %v", label, attempt, broadcastMaxRetries, err)
			if attempt < broadcastMaxRetries {
				sleepCtx(ctx, es.retryBaseDelay*time.Duration(1<<(attempt-1)))
			}
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return fmt.Errorf("%s broadcast: client error %d", label, resp.StatusCode)
		}

		log.Printf("[Events] %s broadcast attempt %d/%d: status %d", label, attempt, broadcastMaxRetries, resp.StatusCode)
		if attempt < broadcastMaxRetries {
			sleepCtx(ctx, es.retryBaseDelay*time.Duration(1<<(attempt-1)))
		}
	}
	return fmt.Errorf("%s broadcast: all %d attempts failed", label, broadcastMaxRetries)
}

// sleepCtx sleeps for d, returning early if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
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

	if err := es.sendBroadcast("call_event", payload); err != nil {
		log.Printf("[Events] Broadcast failed: %v", err)
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

// handleExtensionRing fires when a b-leg is created to ring a specific extension.
// Resolves the extension to a user via the configured resolve URL, then broadcasts
// a targeted incoming_call to that user only.
func (es *EventSubscriber) handleExtensionRing(event *eslgo.Event, callUUID, domainName string) {
	if es.broadcastURL == "" || es.resolveURL == "" {
		return
	}

	extension := eslDecode(event.Headers.Get("Caller-Destination-Number"))
	callerIDNumber := eslDecode(event.Headers.Get("Caller-Caller-ID-Number"))
	callerIDName := eslDecode(event.Headers.Get("Caller-Caller-ID-Name"))
	alegUUID := event.Headers.Get("Other-Leg-Unique-ID")

	if extension == "" || callerIDNumber == "" {
		return
	}

	log.Printf("[Events] Extension ring: %s → %s@%s (call %s, a-leg %s)", callerIDNumber, extension, domainName, callUUID, alegUUID)

	resolved := es.resolveUser(extension, domainName)
	if resolved == nil || len(resolved.users) == 0 {
		return
	}

	// Register this b-leg for all resolved users so answer/hangup events forward to them
	tenants := groupByTenant(resolved.users)
	es.ringMu.Lock()
	es.ringRegistry[callUUID] = &ringRegistration{
		tenants:   tenants,
		callerID:  callerIDNumber,
		extension: extension,
		createdAt: time.Now(),
	}
	es.ringMu.Unlock()

	// Deduplicate: only one ring broadcast per set of users per inbound call.
	// FreeSWITCH creates multiple b-legs for ring groups, retries, and
	// simultaneous ring — each user should see at most one call pop.
	if alegUUID != "" {
		ringKey := alegUUID + ":" + extension + "@" + domainName
		es.seenRingMu.Lock()
		if _, seen := es.seenRing[ringKey]; seen {
			es.seenRingMu.Unlock()
			return
		}
		es.seenRing[ringKey] = time.Now()
		es.seenRingMu.Unlock()
	}

	data := RingCallData{
		CallUUID:          callUUID,
		CallerIDNumber:    callerIDNumber,
		CallerIDName:      callerIDName,
		Extension:         extension,
		Domain:            domainName,
		DestinationNumber: extension,
		Timestamp:         time.Now().Unix(),
	}

	for _, t := range tenants {
		go es.broadcastRing(t, data)
	}
}

// handleRingLifecycle forwards answer/hangup events for tracked b-legs so the
// client can update or dismiss the call pop in real time.
func (es *EventSubscriber) handleRingLifecycle(callUUID, eventName string, event *eslgo.Event) {
	es.ringMu.RLock()
	reg, tracked := es.ringRegistry[callUUID]
	es.ringMu.RUnlock()
	if !tracked {
		return
	}

	var state string
	switch eventName {
	case "CHANNEL_ANSWER":
		state = "answered"
	case "CHANNEL_HANGUP":
		state = "hangup"
	default:
		return
	}

	hangupCause := ""
	if eventName == "CHANNEL_HANGUP" {
		hangupCause = eslDecode(event.Headers.Get("Hangup-Cause"))
	}

	log.Printf("[Events] Call state %s for %s@%s → %d tenant(s) (cause: %s)",
		state, reg.extension, reg.callerID, len(reg.tenants), hangupCause)

	data := CallStateData{
		CallUUID:    callUUID,
		State:       state,
		HangupCause: hangupCause,
		Extension:   reg.extension,
		Timestamp:   time.Now().Unix(),
	}

	for _, t := range reg.tenants {
		go es.broadcastCallState(t, data)
	}
}

// broadcastCallState sends a call state update to the users of one tenant who received the ring.
func (es *EventSubscriber) broadcastCallState(t tenantRecipients, data CallStateData) {
	payload := BroadcastPayload{
		UserUUIDs:  t.userUuids,
		DomainUUID: t.domainUuid,
		Type:       "call_state",
		Data:       data,
	}

	if err := es.sendBroadcast("call_state", payload); err != nil {
		log.Printf("[Events] Call state broadcast failed: %v", err)
	}
}

const userCacheTTL = 5 * time.Minute

// resolveUser looks up the user for an extension via the configured resolve endpoint.
// Results are cached to avoid per-call HTTP requests.
func (es *EventSubscriber) resolveUser(extension, domainName string) *userCacheEntry {
	cacheKey := domainName + ":" + extension

	es.userCacheMu.RLock()
	if entry, ok := es.userCache[cacheKey]; ok && time.Since(entry.resolvedAt) < userCacheTTL {
		es.userCacheMu.RUnlock()
		return entry
	}
	es.userCacheMu.RUnlock()

	payload, err := json.Marshal(map[string]string{
		"p_extension":   extension,
		"p_domain_name": domainName,
	})
	if err != nil {
		log.Printf("[Events] Resolve user for %s@%s: encode request: %v", extension, domainName, err)
		return nil
	}

	req, err := http.NewRequest("POST", es.resolveURL, bytes.NewReader(payload))
	if err != nil {
		log.Printf("[Events] Resolve user for %s@%s: build request: %v", extension, domainName, err)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	if es.resolveSecret != "" {
		req.Header.Set("Authorization", "Bearer "+es.resolveSecret)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[Events] Resolve user failed for %s@%s: %v", extension, domainName, err)
		return nil
	}
	defer resp.Body.Close()

	// Status codes only: the response body is never logged.
	if resp.StatusCode != 200 {
		log.Printf("[Events] Resolve user for %s@%s: status %d; no ring pop", extension, domainName, resp.StatusCode)
		return nil
	}

	var results []struct {
		UserUUID   string `json:"user_uuid"`
		DomainUUID string `json:"domain_uuid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		log.Printf("[Events] Resolve user for %s@%s: response is not a JSON array of users; no ring pop", extension, domainName)
		return nil
	}
	if len(results) == 0 {
		log.Printf("[Events] Resolve user for %s@%s: no users; no ring pop", extension, domainName)
		return nil
	}

	users := make([]resolvedUser, len(results))
	for i, r := range results {
		users[i] = resolvedUser{userUuid: r.UserUUID, domainUuid: r.DomainUUID}
	}

	entry := &userCacheEntry{
		users:      users,
		resolvedAt: time.Now(),
	}

	es.userCacheMu.Lock()
	es.userCache[cacheKey] = entry
	es.userCacheMu.Unlock()

	uuids := make([]string, len(users))
	for i, u := range users {
		uuids[i] = u.userUuid
	}
	log.Printf("[Events] Resolved %s@%s → %d users %v", extension, domainName, len(users), uuids)
	return entry
}

// broadcastRing sends a user-targeted incoming_call event to the WebSocket worker.
// Targets the users of one tenant assigned to the ringing extension, stamped
// with that tenant.
func (es *EventSubscriber) broadcastRing(t tenantRecipients, data RingCallData) {
	payload := BroadcastPayload{
		UserUUIDs:  t.userUuids,
		DomainUUID: t.domainUuid,
		Type:       "incoming_call",
		Data:       data,
	}

	if err := es.sendBroadcast("incoming_call", payload); err != nil {
		log.Printf("[Events] Ring broadcast failed: %v", err)
		return
	}
	log.Printf("[Events] Ring broadcast sent for %s → %d users %v (domain %s)", data.CallerIDNumber, len(t.userUuids), t.userUuids, t.domainUuid)
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

	es.userCacheMu.Lock()
	for key, entry := range es.userCache {
		if entry.resolvedAt.Before(cutoff) {
			delete(es.userCache, key)
		}
	}
	es.userCacheMu.Unlock()

	es.ringMu.Lock()
	for uuid, reg := range es.ringRegistry {
		if reg.createdAt.Before(cutoff) {
			delete(es.ringRegistry, uuid)
		}
	}
	es.ringMu.Unlock()

	es.seenRingMu.Lock()
	for key, seen := range es.seenRing {
		if seen.Before(cutoff) {
			delete(es.seenRing, key)
		}
	}
	es.seenRingMu.Unlock()
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
