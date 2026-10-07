package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/percipia/eslgo"
)

// fakeResolver stands in for the resolve_extension_user RPC: it answers with the
// extension's current owner and counts requests. hold, when set, blocks every
// request until it is closed, so concurrent b-legs can be lined up.
type fakeResolver struct {
	mu       sync.Mutex
	owner    string
	status   int
	requests int32
	hold     chan struct{}
}

func (f *fakeResolver) set(owner string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owner, f.status = owner, status
}

func (f *fakeResolver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&f.requests, 1)
	if f.hold != nil {
		<-f.hold
	}
	f.mu.Lock()
	owner, status := f.owner, f.status
	f.mu.Unlock()
	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	if owner == "" {
		_, _ = w.Write([]byte("[]"))
		return
	}
	_ = json.NewEncoder(w).Encode([]map[string]string{{"user_uuid": owner, "domain_uuid": "tenant-1"}})
}

// fakeWorker records the users each incoming_call broadcast targets.
type fakeWorker struct {
	mu    sync.Mutex
	rings [][]string
}

func (f *fakeWorker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Type      string   `json:"type"`
		UserUUIDs []string `json:"userUuids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&p)
	if p.Type == "incoming_call" {
		f.mu.Lock()
		f.rings = append(f.rings, p.UserUUIDs)
		f.mu.Unlock()
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeWorker) waitForRings(t *testing.T, n int) [][]string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := len(f.rings)
		f.mu.Unlock()
		if got >= n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rings) < n {
		t.Fatalf("expected %d ring broadcasts, got %d", n, len(f.rings))
	}
	return append([][]string(nil), f.rings...)
}

func newRingSubscriber(t *testing.T, owner string) (*EventSubscriber, *fakeResolver, *fakeWorker) {
	t.Helper()
	resolver := &fakeResolver{owner: owner}
	resolveSrv := httptest.NewServer(resolver)
	t.Cleanup(resolveSrv.Close)
	worker := &fakeWorker{}
	workerSrv := httptest.NewServer(worker)
	t.Cleanup(workerSrv.Close)
	es := NewEventSubscriber(EventSubscriberConfig{
		BroadcastURL: workerSrv.URL,
		ResolveURL:   resolveSrv.URL,
	})
	return es, resolver, worker
}

// ring delivers the CHANNEL_CREATE of a b-leg ringing ext@clinic for the call whose
// a-leg is aleg.
func ring(es *EventSubscriber, aleg, bleg, ext string) {
	h := textproto.MIMEHeader{}
	h.Set("Caller-Destination-Number", ext)
	h.Set("Caller-Caller-ID-Number", "5145550100")
	h.Set("Caller-Caller-ID-Name", "Caller")
	if aleg != "" {
		h.Set("Other-Leg-Unique-ID", aleg)
	}
	es.handleExtensionRing(&eslgo.Event{Headers: h}, bleg, "clinic.example.com")
}

func requests(r *fakeResolver) int { return int(atomic.LoadInt32(&r.requests)) }

// Characterization: the b-legs of one call (one per registered device) share one lookup
// and one call pop.
func TestRing_BLegsOfOneCallShareALookup(t *testing.T) {
	es, resolver, worker := newRingSubscriber(t, "user-a")
	ring(es, "call-1", "bleg-1", "101")
	ring(es, "call-1", "bleg-2", "101")
	ring(es, "call-1", "bleg-3", "101")
	rings := worker.waitForRings(t, 1)
	if requests(resolver) != 1 {
		t.Fatalf("expected 1 resolve for 3 b-legs of one call, got %d", requests(resolver))
	}
	if len(rings) != 1 || len(rings[0]) != 1 || rings[0][0] != "user-a" {
		t.Fatalf("unexpected ring broadcasts %v", rings)
	}
}

// Characterization: an unassigned extension pops nothing; once assigned, the next call
// pops for the new owner.
func TestRing_UnassignedThenAssigned(t *testing.T) {
	es, resolver, worker := newRingSubscriber(t, "")
	ring(es, "call-1", "bleg-1", "101")
	resolver.set("user-a", 0)
	ring(es, "call-2", "bleg-2", "101")
	rings := worker.waitForRings(t, 1)
	if rings[0][0] != "user-a" {
		t.Fatalf("expected the new owner, got %v", rings)
	}
}

// Loi 5 U32: a reassignment is seen by the very next call. The former owner gets no
// call pop for a call that starts after the extension moved. Fails with any lookup
// cache that crosses calls (the old 5-minute cache, or a 10-second one).
func TestRing_NextCallAfterReassignmentRingsOnlyTheNewOwner(t *testing.T) {
	es, resolver, worker := newRingSubscriber(t, "user-a")
	ring(es, "call-1", "bleg-1", "101")
	worker.waitForRings(t, 1)
	resolver.set("user-b", 0)
	ring(es, "call-2", "bleg-2", "101")
	rings := worker.waitForRings(t, 2)
	if rings[1][0] != "user-b" {
		t.Fatalf("the call after the reassignment rang %v; want only user-b", rings[1])
	}
	if requests(resolver) != 2 {
		t.Fatalf("expected one lookup per call, got %d", requests(resolver))
	}
}

// Concurrent b-legs of one call (ESL delivers each event on its own goroutine) wait for
// one lookup instead of all missing.
func TestRing_ConcurrentBLegsSingleFlight(t *testing.T) {
	es, resolver, worker := newRingSubscriber(t, "user-a")
	resolver.hold = make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ring(es, "call-1", "bleg-"+string(rune('a'+i)), "101")
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(resolver.hold)
	wg.Wait()
	worker.waitForRings(t, 1)
	if requests(resolver) != 1 {
		t.Fatalf("expected 1 resolve for 5 concurrent b-legs, got %d", requests(resolver))
	}
}

// A failed lookup is not remembered: the next b-leg of the same call tries again.
func TestRing_FailedLookupIsRetried(t *testing.T) {
	es, resolver, worker := newRingSubscriber(t, "user-a")
	resolver.set("user-a", http.StatusServiceUnavailable)
	ring(es, "call-1", "bleg-1", "101")
	resolver.set("user-a", 0)
	ring(es, "call-1", "bleg-2", "101")
	rings := worker.waitForRings(t, 1)
	if rings[0][0] != "user-a" || requests(resolver) != 2 {
		t.Fatalf("rings %v after %d lookups; want user-a after a retry", rings, requests(resolver))
	}
}

// A b-leg with no a-leg (an originated call) has no call to share a lookup with.
func TestRing_NoALegLooksUpEachTime(t *testing.T) {
	es, resolver, _ := newRingSubscriber(t, "user-a")
	ring(es, "", "bleg-1", "101")
	ring(es, "", "bleg-2", "101")
	if requests(resolver) != 2 {
		t.Fatalf("expected 2 lookups without an a-leg, got %d", requests(resolver))
	}
}

// Per-call entries are dropped by the periodic cleanup.
func TestRing_CleanupDropsOldCallEntries(t *testing.T) {
	es, _, worker := newRingSubscriber(t, "user-a")
	ring(es, "call-1", "bleg-1", "101")
	worker.waitForRings(t, 1)
	es.cleanupStaleRegistrations(0)
	es.userCacheMu.Lock()
	n := len(es.userCache)
	es.userCacheMu.Unlock()
	if n != 0 {
		t.Fatalf("expected the cleanup to drop the call's entry, %d left", n)
	}
}
