package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/percipia/eslgo"
)

// Characterization of the three broadcasts fs-api sends to the WebSocket
// worker (call_event, call_state, incoming_call), captured on the wire
// through handleEvent with hand-built ESL events. An inbound a-leg sends
// nothing (call_ringing was removed).

const (
	fxUser1   = "11111111-1111-4111-8111-111111111111"
	fxUser2   = "22222222-2222-4222-8222-222222222222"
	fxUser3   = "33333333-3333-4333-8333-333333333333"
	fxDomainA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	fxDomainB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	fxCallA   = "0a0a0a0a-0000-4000-8000-00000000000a"
	fxBleg1   = "0b0b0b0b-0000-4000-8000-0000000000b1"
	fxBleg2   = "0b0b0b0b-0000-4000-8000-0000000000b2"
	fxInbound = "0c0c0c0c-0000-4000-8000-00000000000c"
)

// captureServer records every request body it receives.
type captureServer struct {
	*httptest.Server
	bodies chan []byte
}

func newCaptureServer(t *testing.T) *captureServer {
	t.Helper()
	cs := &captureServer{bodies: make(chan []byte, 32)}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		cs.bodies <- b
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *captureServer) next(t *testing.T) []byte {
	t.Helper()
	select {
	case b := <-cs.bodies:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a request")
		return nil
	}
}

func (cs *captureServer) none(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case b := <-cs.bodies:
		t.Fatalf("unexpected request: %s", b)
	case <-time.After(wait):
	}
}

// resolveServer answers resolve_extension_user with fixed rows and records each request.
func newResolveServer(t *testing.T, status int, rows []map[string]string) (*httptest.Server, chan *http.Request, chan []byte) {
	t.Helper()
	reqs := make(chan *http.Request, 16)
	bodies := make(chan []byte, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs <- r
		bodies <- b
		w.WriteHeader(status)
		if status == http.StatusOK {
			if err := json.NewEncoder(w).Encode(rows); err != nil {
				t.Errorf("encode rows: %v", err)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, reqs, bodies
}

func newHandlerSubscriber(t *testing.T, cfg EventSubscriberConfig) *EventSubscriber {
	t.Helper()
	es := NewEventSubscriber(cfg)
	es.ctx = context.Background()
	es.retryBaseDelay = time.Millisecond
	return es
}

func eslEvent(headers map[string]string) *eslgo.Event {
	h := textproto.MIMEHeader{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &eslgo.Event{Headers: h}
}

// decodeWire decodes a captured body and replaces data.timestamp (wall clock)
// with a marker after checking it is a number.
func decodeWire(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, raw)
	}
	if data, ok := m["data"].(map[string]any); ok {
		if _, isNum := data["timestamp"].(float64); !isNum {
			t.Fatalf("data.timestamp missing or not a number: %s", raw)
		}
		data["timestamp"] = "<ts>"
	}
	return m
}

func assertWire(t *testing.T, raw []byte, want map[string]any) {
	t.Helper()
	got := decodeWire(t, raw)
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("wire payload mismatch\n got: %s\nwant: %s", g, w)
	}
}

func TestEslDecode(t *testing.T) {
	cases := map[string]string{
		"%2B15145550100": "+15145550100",
		"Alice%20Smith":  "Alice Smith",
		"bad%zzescape":   "bad%zzescape",
	}
	for in, want := range cases {
		if got := eslDecode(in); got != want {
			t.Errorf("eslDecode(%q) = %q, want %q", in, got, want)
		}
	}
}

func originatedCallEvent(name string) *eslgo.Event {
	h := map[string]string{
		"Event-Name":                name,
		"Unique-ID":                 fxCallA,
		"Call-Direction":            "outbound",
		"Caller-Caller-ID-Number":   "101",
		"Caller-Caller-ID-Name":     "Desk",
		"Caller-Destination-Number": "5145550199",
		"Caller-Context":            "acme.example.com",
		"Channel-Call-State":        "ACTIVE",
	}
	if name == "CHANNEL_HANGUP" {
		h["Hangup-Cause"] = "NORMAL_CLEARING"
		h["Channel-Call-State"] = "HANGUP"
	}
	return eslEvent(h)
}

// call_event: originated calls carry the originator's user and domain headers.
func TestCallEventPayload(t *testing.T) {
	worker := newCaptureServer(t)
	es := newHandlerSubscriber(t, EventSubscriberConfig{BroadcastURL: worker.URL})
	es.RegisterCall(&CallRegistration{CallUUID: fxCallA, UserUUID: fxUser1, DomainUUID: fxDomainA, DomainName: "acme.example.com", CreatedAt: time.Now()})

	es.handleEvent(originatedCallEvent("CHANNEL_ANSWER"))
	answered := worker.next(t)
	assertWire(t, answered, map[string]any{
		"userUuid":   fxUser1,
		"domainUuid": fxDomainA,
		"topic":      "calls",
		"type":       "call_event",
		"data": map[string]any{
			"call_uuid":          fxCallA,
			"event":              "CHANNEL_ANSWER",
			"direction":          "outbound",
			"caller_id_number":   "101",
			"caller_id_name":     "Desk",
			"destination_number": "5145550199",
			"context":            "acme.example.com",
			"callstate":          "ACTIVE",
			"timestamp":          "<ts>",
		},
	})
	recordFixture(t, "fs-api.json", "call_event_answer", answered)

	es.handleEvent(originatedCallEvent("CHANNEL_HANGUP"))
	hangup := worker.next(t)
	got := decodeWire(t, hangup)
	if got["data"].(map[string]any)["hangup_cause"] != "NORMAL_CLEARING" {
		t.Fatalf("hangup_cause missing: %s", hangup)
	}
	recordFixture(t, "fs-api.json", "call_event_hangup", hangup)
}

func extensionRingEvent(bleg, aleg string) *eslgo.Event {
	return eslEvent(map[string]string{
		"Event-Name":                "CHANNEL_CREATE",
		"Unique-ID":                 bleg,
		"Call-Direction":            "outbound",
		"Caller-Context":            "acme.example.com",
		"Caller-Destination-Number": "101",
		"Caller-Caller-ID-Number":   "%2B15145550100",
		"Caller-Caller-ID-Name":     "Alice%20Smith",
		"Other-Leg-Unique-ID":       aleg,
	})
}

func lifecycleEvent(name, uuid string) *eslgo.Event {
	h := map[string]string{"Event-Name": name, "Unique-ID": uuid}
	if name == "CHANNEL_HANGUP" {
		h["Hangup-Cause"] = "ORIGINATOR_CANCEL"
	}
	return eslEvent(h)
}

func twoUsersOneTenant() []map[string]string {
	return []map[string]string{
		{"user_uuid": fxUser1, "domain_uuid": fxDomainA},
		{"user_uuid": fxUser2, "domain_uuid": fxDomainA},
	}
}

func usersAny(users ...string) []any {
	out := make([]any, len(users))
	for i, u := range users {
		out[i] = u
	}
	return out
}

func wantIncomingCall(bleg, domain string, users ...string) map[string]any {
	return map[string]any{
		"userUuids":  usersAny(users...),
		"domainUuid": domain,
		"type":       "incoming_call",
		"data": map[string]any{
			"callUuid":          bleg,
			"callerIdNumber":    "+15145550100",
			"callerIdName":      "Alice Smith",
			"extension":         "101",
			"domain":            "acme.example.com",
			"destinationNumber": "101",
			"timestamp":         "<ts>",
		},
	}
}

func wantCallState(bleg, state, cause, domain string, users ...string) map[string]any {
	data := map[string]any{"callUuid": bleg, "state": state, "extension": "101", "timestamp": "<ts>"}
	if cause != "" {
		data["hangupCause"] = cause
	}
	return map[string]any{"userUuids": usersAny(users...), "domainUuid": domain, "type": "call_state", "data": data}
}

// incoming_call + call_state: an extension's b-leg rings every user the
// resolve returns, stamped with the extension's tenant (the resolve's
// domain_uuid); answer/hangup of a tracked b-leg follows to the same users.
func TestExtensionRingPayloads(t *testing.T) {
	worker := newCaptureServer(t)
	resolver, reqs, resolveBodies := newResolveServer(t, http.StatusOK, twoUsersOneTenant())
	es := newHandlerSubscriber(t, EventSubscriberConfig{BroadcastURL: worker.URL, ResolveURL: resolver.URL, ResolveSecret: "resolve-test"})

	es.handleEvent(extensionRingEvent(fxBleg1, fxCallA))

	r := <-reqs
	if got := r.Header.Get("Authorization"); got != "Bearer resolve-test" {
		t.Fatalf("resolve Authorization = %q", got)
	}
	var resolveReq map[string]string
	if err := json.Unmarshal(<-resolveBodies, &resolveReq); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolveReq, map[string]string{"p_extension": "101", "p_domain_name": "acme.example.com"}) {
		t.Fatalf("resolve request = %v", resolveReq)
	}

	ring := worker.next(t)
	assertWire(t, ring, wantIncomingCall(fxBleg1, fxDomainA, fxUser1, fxUser2))
	recordFixture(t, "fs-api.json", "incoming_call", ring)

	// A second b-leg for the same a-leg/extension is tracked but not re-popped.
	es.handleEvent(extensionRingEvent(fxBleg2, fxCallA))
	worker.none(t, 200*time.Millisecond)

	es.handleEvent(lifecycleEvent("CHANNEL_ANSWER", fxBleg1))
	answered := worker.next(t)
	assertWire(t, answered, wantCallState(fxBleg1, "answered", "", fxDomainA, fxUser1, fxUser2))
	recordFixture(t, "fs-api.json", "call_state_answered", answered)

	es.handleEvent(lifecycleEvent("CHANNEL_HANGUP", fxBleg2))
	hangup := worker.next(t)
	assertWire(t, hangup, wantCallState(fxBleg2, "hangup", "ORIGINATOR_CANCEL", fxDomainA, fxUser1, fxUser2))
	recordFixture(t, "fs-api.json", "call_state_hangup", hangup)

	// CHANNEL_DESTROY forgets the b-leg: later lifecycle events send nothing.
	es.handleEvent(lifecycleEvent("CHANNEL_DESTROY", fxBleg1))
	es.handleEvent(lifecycleEvent("CHANNEL_HANGUP", fxBleg1))
	worker.none(t, 200*time.Millisecond)
}

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog redirects the standard logger for the rest of the test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	b := &lockedBuffer{}
	prev := log.Writer()
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(prev) })
	return b
}

// A failed resolve sends nothing and says why in the log (the status code,
// never the secret or the response body).
func TestExtensionRingResolveFailureSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		rows    []map[string]string
		wantLog string
	}{
		{"non-200", http.StatusInternalServerError, nil, "Resolve user for 101@acme.example.com: status 500"},
		{"empty result", http.StatusOK, []map[string]string{}, "Resolve user for 101@acme.example.com: no users"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			worker := newCaptureServer(t)
			resolver, _, _ := newResolveServer(t, tc.status, tc.rows)
			es := newHandlerSubscriber(t, EventSubscriberConfig{BroadcastURL: worker.URL, ResolveURL: resolver.URL, ResolveSecret: "resolve-test-secret"})
			es.handleEvent(extensionRingEvent(fxBleg1, fxCallA))
			es.handleEvent(lifecycleEvent("CHANNEL_ANSWER", fxBleg1))
			worker.none(t, 200*time.Millisecond)
			if got := logs.String(); !strings.Contains(got, tc.wantLog) || strings.Contains(got, "resolve-test-secret") {
				t.Fatalf("log = %q, want %q and no secret", got, tc.wantLog)
			}
		})
	}
}

func TestGroupByTenant(t *testing.T) {
	got := groupByTenant([]resolvedUser{
		{userUuid: fxUser1, domainUuid: fxDomainA},
		{userUuid: fxUser2, domainUuid: fxDomainB},
		{userUuid: fxUser3, domainUuid: fxDomainA},
	})
	want := []tenantRecipients{
		{domainUuid: fxDomainA, userUuids: []string{fxUser1, fxUser3}},
		{domainUuid: fxDomainB, userUuids: []string{fxUser2}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groupByTenant = %+v, want %+v", got, want)
	}
}

// nextByDomain collects n bodies (sent from concurrent goroutines, so in any
// order) and indexes them by domainUuid.
func nextByDomain(t *testing.T, cs *captureServer, n int) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for i := 0; i < n; i++ {
		b := cs.next(t)
		var m struct {
			DomainUUID string `json:"domainUuid"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if _, dup := out[m.DomainUUID]; dup {
			t.Fatalf("two broadcasts for domain %q", m.DomainUUID)
		}
		out[m.DomainUUID] = b
	}
	return out
}

// A resolve whose users span tenants (domain_name uniqueness is data-only)
// sends one incoming_call and one call_state per tenant, each stamped with its
// own tenant and carrying only that tenant's users.
func TestExtensionRingSplitsPerTenant(t *testing.T) {
	worker := newCaptureServer(t)
	resolver, _, _ := newResolveServer(t, http.StatusOK, []map[string]string{
		{"user_uuid": fxUser1, "domain_uuid": fxDomainA},
		{"user_uuid": fxUser2, "domain_uuid": fxDomainB},
		{"user_uuid": fxUser3, "domain_uuid": fxDomainA},
	})
	es := newHandlerSubscriber(t, EventSubscriberConfig{BroadcastURL: worker.URL, ResolveURL: resolver.URL})

	es.handleEvent(extensionRingEvent(fxBleg1, fxCallA))
	rings := nextByDomain(t, worker, 2)
	assertWire(t, rings[fxDomainA], wantIncomingCall(fxBleg1, fxDomainA, fxUser1, fxUser3))
	assertWire(t, rings[fxDomainB], wantIncomingCall(fxBleg1, fxDomainB, fxUser2))
	worker.none(t, 100*time.Millisecond)

	es.handleEvent(lifecycleEvent("CHANNEL_HANGUP", fxBleg1))
	states := nextByDomain(t, worker, 2)
	assertWire(t, states[fxDomainA], wantCallState(fxBleg1, "hangup", "ORIGINATOR_CANCEL", fxDomainA, fxUser1, fxUser3))
	assertWire(t, states[fxDomainB], wantCallState(fxBleg1, "hangup", "ORIGINATOR_CANCEL", fxDomainB, fxUser2))
	recordFixture(t, "fs-api.json", "incoming_call_split_tenant_b", rings[fxDomainB])
	recordFixture(t, "fs-api.json", "call_state_split_tenant_b", states[fxDomainB])
	worker.none(t, 100*time.Millisecond)
}

func inboundALegEvent() *eslgo.Event {
	return eslEvent(map[string]string{
		"Event-Name":                "CHANNEL_CREATE",
		"Unique-ID":                 fxInbound,
		"Call-Direction":            "inbound",
		"Caller-Context":            "public",
		"Caller-Caller-ID-Number":   "%2B15145550100",
		"Caller-Caller-ID-Name":     "Alice%20Smith",
		"Caller-Destination-Number": "5145550199",
	})
}

// An inbound a-leg (CHANNEL_CREATE, public context) sends nothing: the
// call_ringing broadcast and the inbound webhook were removed (no consumer).
func TestInboundALegSendsNothing(t *testing.T) {
	worker := newCaptureServer(t)
	resolver, reqs, _ := newResolveServer(t, http.StatusOK, twoUsersOneTenant())
	es := newHandlerSubscriber(t, EventSubscriberConfig{BroadcastURL: worker.URL, ResolveURL: resolver.URL})
	es.handleEvent(inboundALegEvent())
	worker.none(t, 200*time.Millisecond)
	select {
	case <-reqs:
		t.Fatal("an inbound a-leg triggered a resolve")
	default:
	}
}
