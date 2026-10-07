package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func startSubscriber(t *testing.T, es *EventSubscriber) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		es.Start(ctx)
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("Start did not return after cancel")
		}
	}
}

func waitSubscribed(t *testing.T, fs *fakeFreeSWITCH, want int) {
	t.Helper()
	select {
	case n := <-fs.subscribed:
		if n != want {
			t.Fatalf("subscription on connection %d, want %d", n, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no event subscription on connection %d", want)
	}
}

// The event subscription must survive a FreeSWITCH connection that ends
// without a disconnect-notice. eslgo never reported that case, so the
// subscriber used to wait on a dead connection forever (f1, f1-dev).
func TestEventSubscriberRedialsDeadConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		// what connection 1 does on its 4th probe: drop the socket without a
		// notice, or stay open and silent
		closeSocket bool
	}{
		{"socket dropped without a disconnect-notice", true},
		{"peer stops answering", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			probes := map[int]int{}
			var unexpected []string
			fs := newFakeFreeSWITCH(t, "ClueCon-test", func(connNo int, cmd string, conn net.Conn) bool {
				mu.Lock()
				defer mu.Unlock()
				if cmd != "status" {
					unexpected = append(unexpected, cmd)
					return true
				}
				probes[connNo]++
				if connNo == 1 && probes[connNo] >= 4 {
					return tc.closeSocket
				}
				io.WriteString(conn, apiResponseFrame("UP 0 years, 0 days\n"))
				return false
			})
			host, port := fs.hostPort()
			es := NewEventSubscriber(EventSubscriberConfig{ESLHost: host, ESLPort: port, ESLPassword: "ClueCon-test"})
			es.eventProbe = probeTiming{interval: 40 * time.Millisecond, timeout: 150 * time.Millisecond}
			es.reconnectDelay = 20 * time.Millisecond
			stop := startSubscriber(t, es)

			waitSubscribed(t, fs, 1)
			waitSubscribed(t, fs, 2)
			stop()

			mu.Lock()
			defer mu.Unlock()
			if probes[1] < 4 {
				t.Fatalf("connection 1 replaced after %d probes; answered probes must keep it", probes[1])
			}
			if len(unexpected) > 0 {
				t.Fatalf("unexpected api commands on the event connection: %q", unexpected)
			}
		})
	}
}

// A disconnect-notice also ends the connection and is redialed.
func TestEventSubscriberRedialsAfterDisconnectNotice(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(connNo int, _ string, conn net.Conn) bool {
		if connNo == 1 {
			io.WriteString(conn, disconnectNoticeFrame())
			return true
		}
		io.WriteString(conn, apiResponseFrame("UP\n"))
		return false
	})
	host, port := fs.hostPort()
	es := NewEventSubscriber(EventSubscriberConfig{ESLHost: host, ESLPort: port, ESLPassword: "ClueCon-test"})
	es.eventProbe = probeTiming{interval: 40 * time.Millisecond, timeout: time.Second}
	es.reconnectDelay = 20 * time.Millisecond
	stop := startSubscriber(t, es)
	waitSubscribed(t, fs, 1)
	waitSubscribed(t, fs, 2)
	stop()
}

func eventPlainFrame(headers [][2]string) string {
	body := ""
	for _, h := range headers {
		body += h[0] + ": " + h[1] + "\n"
	}
	body += "\n"
	return fmt.Sprintf("Content-Length: %d\nContent-Type: text/event-plain\n\n%s", len(body), body)
}

// Events read from the connection reach handleEvent with their header values
// still URL-encoded, exactly as eslgo delivered them (call_event forwards
// them as-is; the other handlers decode what they use).
func TestEventConnectionDeliversEvents(t *testing.T) {
	worker := newCaptureServer(t)
	frame := eventPlainFrame([][2]string{
		{"Event-Name", "CHANNEL_ANSWER"},
		{"Unique-ID", fxCallA},
		{"Call-Direction", "outbound"},
		{"Caller-Caller-ID-Number", "101"},
		{"Caller-Caller-ID-Name", "Front%20Desk"},
		{"Caller-Destination-Number", "%2B15145550199"},
		{"Caller-Context", "acme.example.com"},
		{"Channel-Call-State", "ACTIVE"},
	})
	var once sync.Once
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, _ string, conn net.Conn) bool {
		io.WriteString(conn, apiResponseFrame("UP\n"))
		once.Do(func() { io.WriteString(conn, frame) })
		return false
	})
	host, port := fs.hostPort()
	es := NewEventSubscriber(EventSubscriberConfig{ESLHost: host, ESLPort: port, ESLPassword: "ClueCon-test", BroadcastURL: worker.URL})
	es.eventProbe = probeTiming{interval: 20 * time.Millisecond, timeout: time.Second}
	es.RegisterCall(&CallRegistration{CallUUID: fxCallA, UserUUID: fxUser1, DomainUUID: fxDomainA, DomainName: "acme.example.com", CreatedAt: time.Now()})
	stop := startSubscriber(t, es)
	defer stop()

	waitSubscribed(t, fs, 1)
	assertWire(t, worker.next(t), map[string]any{
		"userUuid":   fxUser1,
		"domainUuid": fxDomainA,
		"topic":      "calls",
		"type":       "call_event",
		"data": map[string]any{
			"call_uuid":          fxCallA,
			"event":              "CHANNEL_ANSWER",
			"direction":          "outbound",
			"caller_id_number":   "101",
			"caller_id_name":     "Front%20Desk",
			"destination_number": "%2B15145550199",
			"context":            "acme.example.com",
			"callstate":          "ACTIVE",
			"timestamp":          "<ts>",
		},
	})
	if got := fs.commands()[1]; got != "1:"+eventSubscription {
		t.Fatalf("subscription command = %q", got)
	}
}

func TestParsePlainEvent(t *testing.T) {
	ev, err := parsePlainEvent([]byte("Event-Name: CHANNEL_CREATE\nUnique-ID: " + fxCallA + "\nvariable_domain_uuid: " + fxDomainA + "\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Headers.Get("Unique-ID") != fxCallA || ev.Headers.Get("variable_domain_uuid") != fxDomainA {
		t.Fatalf("headers = %v", ev.Headers)
	}
	if _, err := parsePlainEvent([]byte("not a header line\n\n")); err == nil {
		t.Fatal("malformed event parsed")
	}
}

// An event frame over the body cap is read, discarded and logged; the
// connection stays up and later events are still delivered.
func TestEventConnectionSkipsOversizedFrame(t *testing.T) {
	logs := captureLog(t)
	worker := newCaptureServer(t)
	big := strings.Repeat("z", maxESLBody+1)
	oversized := fmt.Sprintf("Content-Length: %d\nContent-Type: text/event-plain\n\n%s", len(big), big)
	frame := eventPlainFrame([][2]string{
		{"Event-Name", "CHANNEL_ANSWER"},
		{"Unique-ID", fxCallA},
		{"Channel-Call-State", "ACTIVE"},
	})
	var once sync.Once
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, _ string, conn net.Conn) bool {
		io.WriteString(conn, apiResponseFrame("UP\n"))
		once.Do(func() { io.WriteString(conn, oversized+frame) })
		return false
	})
	host, port := fs.hostPort()
	es := NewEventSubscriber(EventSubscriberConfig{ESLHost: host, ESLPort: port, ESLPassword: "ClueCon-test", BroadcastURL: worker.URL})
	es.eventProbe = probeTiming{interval: 20 * time.Millisecond, timeout: time.Second}
	es.RegisterCall(&CallRegistration{CallUUID: fxCallA, UserUUID: fxUser1, DomainUUID: fxDomainA, CreatedAt: time.Now()})
	stop := startSubscriber(t, es)
	defer stop()

	waitSubscribed(t, fs, 1)
	if got := decodeWire(t, worker.next(t)); got["type"] != "call_event" {
		t.Fatalf("got %v", got)
	}
	if n := fs.acceptCount(); n != 1 {
		t.Fatalf("connection replaced after an oversized frame: %d dials", n)
	}
	if !strings.Contains(logs.String(), "Skipped an oversized ESL frame") {
		t.Fatalf("no skip log: %q", logs.String())
	}
}
