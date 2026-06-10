package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestSubscriber returns an EventSubscriber wired to point its broadcasts at
// the given worker URL, with a short timeout and near-zero backoff so the retry
// state machine exercises quickly.
func newTestSubscriber(t *testing.T, workerURL string) *EventSubscriber {
	t.Helper()
	orig := broadcastRetryBaseDelay
	broadcastRetryBaseDelay = time.Millisecond
	t.Cleanup(func() { broadcastRetryBaseDelay = orig })

	return &EventSubscriber{
		broadcastURL: workerURL,
		ctx:          context.Background(),
		httpClient:   &http.Client{Timeout: 2 * time.Second},
	}
}

func testPayload() BroadcastPayload {
	return BroadcastPayload{
		UserUUIDs: []string{"user-1"},
		Type:      "incoming_call",
		Data:      RingCallData{CallUUID: "call-1", Extension: "1001"},
	}
}

func TestSendBroadcast_SucceedsFirstAttempt(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	es := newTestSubscriber(t, srv.URL)
	if err := es.sendBroadcast("incoming_call", testPayload()); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 request, got %d", got)
	}
}

func TestSendBroadcast_RetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fail the first two attempts with a 5xx, succeed on the third.
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	es := newTestSubscriber(t, srv.URL)
	if err := es.sendBroadcast("incoming_call", testPayload()); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 requests (2 retries), got %d", got)
	}
}

func TestSendBroadcast_GivesUpAfterMaxRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	es := newTestSubscriber(t, srv.URL)
	if err := es.sendBroadcast("call_state", testPayload()); err == nil {
		t.Fatal("expected an error after exhausting retries, got nil")
	}
	if got := atomic.LoadInt32(&calls); got != broadcastMaxRetries {
		t.Fatalf("expected %d attempts, got %d", broadcastMaxRetries, got)
	}
}

func TestSendBroadcast_NoRetryOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest) // permanent client error
	}))
	defer srv.Close()

	es := newTestSubscriber(t, srv.URL)
	if err := es.sendBroadcast("incoming_call", testPayload()); err == nil {
		t.Fatal("expected an error on 4xx, got nil")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("4xx must not be retried: expected 1 request, got %d", got)
	}
}

func TestSendBroadcast_StopsWhenContextCancelled(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	es := newTestSubscriber(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled — must not attempt any request
	es.ctx = ctx

	if err := es.sendBroadcast("incoming_call", testPayload()); err == nil {
		t.Fatal("expected an error when context is cancelled, got nil")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("cancelled context must short-circuit: expected 0 requests, got %d", got)
	}
}

// X-Worker-Auth must be sent when a secret is configured, and omitted otherwise.
func TestSendBroadcast_SetsAuthHeaderWhenSecretPresent(t *testing.T) {
	gotAuth := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("X-Worker-Auth")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	es := newTestSubscriber(t, srv.URL)
	es.broadcastSecret = "s3cr3t"
	if err := es.sendBroadcast("incoming_call", testPayload()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h := <-gotAuth; h != "s3cr3t" {
		t.Fatalf("expected auth header 's3cr3t', got %q", h)
	}
}
