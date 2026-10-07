package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Each originate runs on its own ESL connection, so its reply can only be
// its own: the handler registers whatever canonical uuid that reply names.

var alegIndex = regexp.MustCompile(`user/(\d+)@`)

func originateHandlerFor(fs *fakeFreeSWITCH) (*APIHandler, *EventSubscriber) {
	host, port := fs.hostPort()
	es := NewEventSubscriber(EventSubscriberConfig{})
	return &APIHandler{originateESL: newESLOneShot(host, port, "ClueCon-test"), originateSlots: make(chan struct{}, maxConcurrentOriginates), eventSubscriber: es}, es
}

func originate(t *testing.T, h *APIHandler, user, domain string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/calls/originate", bytes.NewReader(b))
	req.Header.Set("X-User-UUID", user)
	req.Header.Set("X-Domain-UUID", domain)
	rec := httptest.NewRecorder()
	h.OriginateCall(rec, req)
	return rec
}

func registeredCalls(es *EventSubscriber) map[string]*CallRegistration {
	es.mu.RLock()
	defer es.mu.RUnlock()
	out := make(map[string]*CallRegistration, len(es.registry))
	for k, v := range es.registry {
		out[k] = v
	}
	return out
}

func replyWith(reply string) func(int, string, net.Conn) bool {
	return func(_ int, _ string, conn net.Conn) bool {
		io.WriteString(conn, apiResponseFrame(reply))
		return false
	}
}

// Concurrent originates, answered out of order, each register the uuid of
// their own reply under their own user and tenant, on one connection each.
func TestConcurrentOriginatesRegisterTheirOwnReply(t *testing.T) {
	const n = 20
	var mu sync.Mutex
	legOf := map[string]string{} // aleg index -> uuid replied
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, cmd string, conn net.Conn) bool {
		m := alegIndex.FindStringSubmatch(cmd)
		if m == nil || !strings.HasPrefix(cmd, "originate ") {
			io.WriteString(conn, apiResponseFrame("-ERR bad command\n"))
			return true
		}
		leg := fmt.Sprintf("0e0e0e0e-0000-4000-8000-%012d", rand.IntN(1_000_000_000))
		mu.Lock()
		legOf[m[1]] = leg
		mu.Unlock()
		time.Sleep(time.Duration(rand.IntN(30)) * time.Millisecond)
		io.WriteString(conn, apiResponseFrame("+OK "+leg+"\n"))
		return true // FreeSWITCH would keep it; the client closes after one reply anyway
	})
	h, es := originateHandlerFor(fs)
	var wg sync.WaitGroup
	users := map[string]string{}
	for i := 0; i < n; i++ {
		user, domain := fxUser1, fxDomainA
		if i%2 == 1 {
			user, domain = fxUser2, fxDomainB
		}
		users[fmt.Sprint(i)] = user
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := originate(t, h, user, domain, map[string]any{"aleg": fmt.Sprintf("user/%d@acme.example.com", i)})
			if rec.Code != http.StatusOK {
				t.Errorf("status %d: %s", rec.Code, rec.Body)
			}
		}()
	}
	wg.Wait()
	if got := fs.acceptCount(); got != n {
		t.Fatalf("connections = %d, want one per originate (%d)", got, n)
	}
	reg := registeredCalls(es)
	if len(reg) != n {
		t.Fatalf("registered %d calls, want %d", len(reg), n)
	}
	for i, leg := range legOf {
		r := reg[leg]
		if r == nil || r.UserUUID != users[i] {
			t.Fatalf("leg %s of request %s registered as %+v, want user %s", leg, i, r, users[i])
		}
		wantDomain := map[string]string{fxUser1: fxDomainA, fxUser2: fxDomainB}[r.UserUUID]
		if r.DomainUUID != wantDomain {
			t.Fatalf("leg %s registered with domain %s", leg, r.DomainUUID)
		}
	}
}

// A forked A-leg (user/<ext>@<domain> rings every registration) answers with
// the uuid of whichever leg picked up: that leg is registered. No
// origination_uuid is added to the dial string.
func TestOriginateRegistersTheAnsweringForkLeg(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", replyWith("+OK "+fxCallY+"\n"))
	h, es := originateHandlerFor(fs)
	rec := originate(t, h, fxUser1, fxDomainA, map[string]any{"aleg": "user/101@acme.example.com"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), fxCallY) {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if r := registeredCalls(es)[fxCallY]; r == nil || r.UserUUID != fxUser1 || r.DomainUUID != fxDomainA {
		t.Fatalf("registry = %+v", registeredCalls(es))
	}
	cmds := fs.commands()
	if len(cmds) != 2 || strings.Contains(cmds[1], "origination_uuid") || cmds[1] != "1:api originate {originate_timeout=60}user/101@acme.example.com &park() undef undef undef undef 60" {
		t.Fatalf("commands = %q", cmds)
	}
}

// A caller-supplied origination_uuid is passed through unchanged, as in HEAD.
func TestOriginatePassesCallerOriginationUUIDThrough(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", replyWith("+OK "+fxCallX+"\n"))
	h, es := originateHandlerFor(fs)
	rec := originate(t, h, fxUser1, fxDomainA, map[string]any{
		"aleg":              "user/101@acme.example.com",
		"channel_variables": map[string]any{"origination_uuid": fxCallX},
	})
	if rec.Code != http.StatusOK || registeredCalls(es)[fxCallX] == nil {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if cmd := fs.commands()[1]; cmd != "1:api originate {originate_timeout=60,origination_uuid="+fxCallX+"}user/101@acme.example.com &park() undef undef undef undef 60" {
		t.Fatalf("command %q", cmd)
	}
}

// A connection error answers 502 and registers nothing.
func TestOriginateConnectionErrorRegistersNothing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	es := NewEventSubscriber(EventSubscriberConfig{})
	h := &APIHandler{originateESL: newESLOneShot(host, port, "ClueCon-test"), originateSlots: make(chan struct{}, maxConcurrentOriginates), eventSubscriber: es}
	rec := originate(t, h, fxUser1, fxDomainA, map[string]any{"aleg": "user/101@acme.example.com"})
	if rec.Code != http.StatusBadGateway || len(registeredCalls(es)) != 0 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

// The connection closing before the reply is a transport failure: 502, nothing registered.
func TestOriginateConnectionDroppedRegistersNothing(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(int, string, net.Conn) bool { return true })
	h, es := originateHandlerFor(fs)
	rec := originate(t, h, fxUser1, fxDomainA, map[string]any{"aleg": "user/101@acme.example.com"})
	if rec.Code != http.StatusBadGateway || len(registeredCalls(es)) != 0 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

// A FreeSWITCH -ERR is a call outcome: 409 with the cause, nothing registered.
func TestOriginateErrReplyIsConflictWithCause(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", replyWith("-ERR USER_BUSY\n"))
	h, es := originateHandlerFor(fs)
	rec := originate(t, h, fxUser1, fxDomainA, map[string]any{"aleg": "user/101@acme.example.com"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Call not placed: USER_BUSY") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(registeredCalls(es)) != 0 {
		t.Fatalf("registered %v", registeredCalls(es))
	}
}

// A reply that is neither +OK nor -ERR is a gateway fault: 502.
func TestOriginateUnexpectedReply(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", replyWith("garbage\n"))
	h, es := originateHandlerFor(fs)
	rec := originate(t, h, fxUser1, fxDomainA, map[string]any{"aleg": "user/101@acme.example.com"})
	if rec.Code != http.StatusBadGateway || len(registeredCalls(es)) != 0 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

// +OK naming something that is not a canonical uuid: the call was placed
// (200 as in HEAD) but is not tracked.
func TestOriginateNonCanonicalReplyIsNotTracked(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", replyWith("+OK MY-CALL\n"))
	h, es := originateHandlerFor(fs)
	rec := originate(t, h, fxUser1, fxDomainA, map[string]any{"aleg": "user/101@acme.example.com"})
	if rec.Code != http.StatusOK || len(registeredCalls(es)) != 0 {
		t.Fatalf("status %d: %s; registry %v", rec.Code, rec.Body, registeredCalls(es))
	}
}

// In-flight originates are capped: when every slot is held, originate answers
// 503 with Retry-After at once (no queueing), and a freed slot is reusable.
func TestOriginateCapAnswers503WhenFull(t *testing.T) {
	release := make(chan struct{})
	received := make(chan struct{}, 8)
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, _ string, conn net.Conn) bool {
		received <- struct{}{}
		<-release
		io.WriteString(conn, apiResponseFrame("+OK "+fxCallX+"\n"))
		return true
	})
	h, _ := originateHandlerFor(fs)
	h.originateSlots = make(chan struct{}, 2)

	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			codes <- originate(t, h, fxUser1, fxDomainA, map[string]any{"aleg": "user/101@acme.example.com"}).Code
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-received:
		case <-time.After(3 * time.Second):
			t.Fatal("originates did not reach FreeSWITCH")
		}
	}

	start := time.Now()
	rec := originate(t, h, fxUser1, fxDomainA, map[string]any{"aleg": "user/101@acme.example.com"})
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != originateRetryAfter {
		t.Fatalf("status %d, Retry-After %q: %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("the refusal waited %s instead of answering at once", time.Since(start))
	}
	if n := fs.acceptCount(); n != 2 {
		t.Fatalf("the refused originate reached FreeSWITCH (%d dials)", n)
	}

	close(release)
	for i := 0; i < 2; i++ {
		if c := <-codes; c != http.StatusOK {
			t.Fatalf("held originate answered %d", c)
		}
	}
	if c := originate(t, h, fxUser1, fxDomainA, map[string]any{"aleg": "user/101@acme.example.com"}).Code; c != http.StatusOK {
		t.Fatalf("after release: %d", c)
	}
}

// A request refused before dialing (bad body) holds no slot.
func TestOriginateBadRequestHoldsNoSlot(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", replyWith("+OK "+fxCallX+"\n"))
	h, _ := originateHandlerFor(fs)
	h.originateSlots = make(chan struct{}, 1)
	if c := originate(t, h, fxUser1, fxDomainA, map[string]any{}).Code; c != http.StatusBadRequest {
		t.Fatalf("status %d", c)
	}
	if len(h.originateSlots) != 0 {
		t.Fatal("slot leaked")
	}
	if c := originate(t, h, fxUser1, fxDomainA, map[string]any{"aleg": "user/101@acme.example.com"}).Code; c != http.StatusOK || len(h.originateSlots) != 0 {
		t.Fatalf("status %d, slots held %d", c, len(h.originateSlots))
	}
}

// A FreeSWITCH -USAGE reply (malformed arguments) is a client error: 400.
func TestOriginateUsageReplyIsBadRequest(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", replyWith("-USAGE: <call url> <exten>|&<application_name>(<app_args>)\n"))
	h, es := originateHandlerFor(fs)
	rec := originate(t, h, fxUser1, fxDomainA, map[string]any{"aleg": "user/101@acme.example.com"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Invalid originate arguments: -USAGE") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(registeredCalls(es)) != 0 {
		t.Fatalf("registered %v", registeredCalls(es))
	}
}

// timeout_sec above 85 and originate_timeout in channel_variables are refused
// before anything is sent; 85 itself and the Chrome extension's 30 pass.
func TestOriginateTimeoutBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"timeout_sec 30 (Chrome extension)", map[string]any{"aleg": "user/101@acme.example.com", "timeout_sec": 30}, http.StatusOK},
		{"timeout_sec 85", map[string]any{"aleg": "user/101@acme.example.com", "timeout_sec": 85}, http.StatusOK},
		{"timeout_sec 86", map[string]any{"aleg": "user/101@acme.example.com", "timeout_sec": 86}, http.StatusBadRequest},
		{"originate_timeout variable", map[string]any{"aleg": "user/101@acme.example.com", "channel_variables": map[string]any{"originate_timeout": 300}}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeFreeSWITCH(t, "ClueCon-test", replyWith("+OK "+fxCallX+"\n"))
			h, _ := originateHandlerFor(fs)
			rec := originate(t, h, fxUser1, fxDomainA, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			if tc.want == http.StatusBadRequest && (fs.acceptCount() != 0 || len(h.originateSlots) != 0) {
				t.Fatalf("refused request reached FreeSWITCH (%d dials) or held a slot", fs.acceptCount())
			}
		})
	}
}

// The deadline always outlasts the ring timeout FreeSWITCH was given, and the
// longest one still fits the server's WriteTimeout.
func TestOriginateDeadline(t *testing.T) {
	for _, requested := range []int{0, 1, 30, maxOriginateTimeoutSec} {
		ring := originateTimeoutSec(requested)
		if requested == 0 && ring != 60 {
			t.Fatalf("no timeout_sec must send FreeSWITCH's default 60, got %d", ring)
		}
		d := originateDeadline(ring)
		if d <= time.Duration(ring)*time.Second {
			t.Fatalf("deadline %s does not outlast the %d s ring timeout", d, ring)
		}
		if d >= serverWriteTimeout {
			t.Fatalf("deadline %s is not under the %s WriteTimeout", d, serverWriteTimeout)
		}
	}
}

// The exact command for every combination of optional arguments. FreeSWITCH
// reads originate's optional arguments by position, with "undef" for a
// skipped slot, so every slot is written and the ring timeout is always the
// seventh argument (U48: {aleg, timeout_sec:10} used to send
// "... &park() 10", which FreeSWITCH read as the dialplan, ringing 60 s).
func TestOriginateCommandArguments(t *testing.T) {
	const aleg = "user/101@acme.example.com"
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"none", map[string]any{"aleg": aleg},
			"originate {originate_timeout=60}user/101@acme.example.com &park() undef undef undef undef 60"},
		{"timeout only", map[string]any{"aleg": aleg, "timeout_sec": 10},
			"originate {originate_timeout=10}user/101@acme.example.com &park() undef undef undef undef 10"},
		{"context only", map[string]any{"aleg": aleg, "context": "acme.example.com"},
			"originate {originate_timeout=60}user/101@acme.example.com &park() undef acme.example.com undef undef 60"},
		{"dialplan only", map[string]any{"aleg": aleg, "dialplan": "XML"},
			"originate {originate_timeout=60}user/101@acme.example.com &park() XML undef undef undef 60"},
		{"cid only", map[string]any{"aleg": aleg, "caller_id_name": "Front Desk", "caller_id_number": "5145550100"},
			"originate {originate_timeout=60,origination_caller_id_number=5145550100,origination_caller_id_name='Front Desk'}user/101@acme.example.com &park() undef undef undef undef 60"},
		{"all", map[string]any{"aleg": aleg, "bleg": "5145550199", "dialplan": "XML", "context": "acme.example.com",
			"caller_id_name": "Front Desk", "caller_id_number": "5145550100", "timeout_sec": 25},
			"originate {originate_timeout=25,origination_caller_id_number=5145550100,origination_caller_id_name='Front Desk'}user/101@acme.example.com 5145550199 XML acme.example.com undef undef 25"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeFreeSWITCH(t, "ClueCon-test", replyWith("+OK "+fxCallX+"\n"))
			h, _ := originateHandlerFor(fs)
			if rec := originate(t, h, fxUser1, fxDomainA, tc.body); rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			cmds := fs.commands()
			if len(cmds) != 2 || cmds[1] != "1:api "+tc.want {
				t.Fatalf("command\n got: %q\nwant: %q", cmds, "1:api "+tc.want)
			}
			// FreeSWITCH sees exactly seven arguments, the ring timeout last,
			// and the same timeout first in the variables every endpoint inherits.
			args := fsOriginateArgs(t, tc.want)
			if fields := strings.Fields(tc.want); len(args) != 7 || args[6] != fields[len(fields)-1] || !strings.HasPrefix(args[0], "{originate_timeout="+args[6]) {
				t.Fatalf("FreeSWITCH would see %d args %q", len(args), args)
			}
		})
	}
}

// fsOriginateArgs splits an `api originate` argument string the way
// FreeSWITCH does (switch_utils.c separate_string_blank_delim +
// cleanup_separated_string: spaces separate, single quotes group and are
// stripped; this test only ever sees inputs without backslashes or "^^").
func fsOriginateArgs(t *testing.T, cmd string) []string {
	t.Helper()
	rest, ok := strings.CutPrefix(cmd, "originate ")
	if !ok {
		t.Fatalf("not an originate: %q", cmd)
	}
	var args []string
	var cur strings.Builder
	inQuotes, inToken := false, false
	for _, r := range rest {
		switch {
		case r == '\'':
			inQuotes = !inQuotes
			inToken = true
		case r == ' ' && !inQuotes:
			if inToken {
				args = append(args, cur.String())
				cur.Reset()
				inToken = false
			}
		default:
			cur.WriteRune(r)
			inToken = true
		}
	}
	if inToken {
		args = append(args, cur.String())
	}
	return args
}

// The Chrome extension's real request: its &transfer(dest XML domain) B-leg
// has spaces, so it is sent single-quoted as one argument, and FreeSWITCH sees
// exactly seven arguments with the requested ring timeout last. (Unquoted, the
// fixed seven-slot command would have been ten arguments: -USAGE.)
func TestOriginateChromeExtensionRequest(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", replyWith("+OK "+fxCallX+"\n"))
	h, es := originateHandlerFor(fs)
	rec := originate(t, h, fxUser1, fxDomainA, map[string]any{
		"aleg":             "user/101@acme.example.com",
		"bleg":             "&transfer(5145550199 XML acme.example.com)",
		"caller_id_name":   "5145550199",
		"caller_id_number": "5145550199",
		"timeout_sec":      30,
	})
	if rec.Code != http.StatusOK || registeredCalls(es)[fxCallX] == nil {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	want := "originate {originate_timeout=30,origination_caller_id_number=5145550199,origination_caller_id_name='5145550199'}user/101@acme.example.com '&transfer(5145550199 XML acme.example.com)' undef undef undef undef 30"
	cmd := strings.TrimPrefix(fs.commands()[1], "1:api ")
	if cmd != want {
		t.Fatalf("command\n got: %q\nwant: %q", cmd, want)
	}
	args := fsOriginateArgs(t, cmd)
	if len(args) != 7 || args[1] != "&transfer(5145550199 XML acme.example.com)" || args[6] != "30" {
		t.Fatalf("FreeSWITCH would see %d args %q", len(args), args)
	}
}

// Every route around the ring timeout is refused with 400 before anything is
// dialed: inline variables in aleg/bleg, argument-shifting characters, and
// channel variables that change originate timing (any case).
func TestOriginateRefusesTimeoutOverrides(t *testing.T) {
	const aleg = "user/101@acme.example.com"
	cases := map[string]map[string]any{
		"aleg {originate_timeout}":    {"aleg": "{originate_timeout=300}" + aleg},
		"aleg [leg_timeout]":          {"aleg": "[leg_timeout=300]" + aleg},
		"aleg <leg_timeout>":          {"aleg": "<leg_timeout=300>" + aleg},
		"aleg space":                  {"aleg": aleg + " 300"},
		"aleg tab":                    {"aleg": aleg + "\t300"},
		"aleg newline":                {"aleg": aleg + "\n"},
		"aleg quote":                  {"aleg": aleg + "'"},
		"aleg backslash":              {"aleg": aleg + `\ 300`},
		"aleg delimiter switch":       {"aleg": "^^:" + aleg},
		"aleg serial failover":        {"aleg": aleg + "|user/102@acme.example.com"},
		"aleg failover in enterprise": {"aleg": aleg + ":_:user/102@acme.example.com|user/103@acme.example.com"},
		"aleg variable expansion":     {"aleg": "${group_call(sales@acme.example.com+F)}"},
		"aleg group/ endpoint":        {"aleg": "group/sales@acme.example.com"},
		"aleg GROUP/ after a comma":   {"aleg": aleg + ",GROUP/sales@acme.example.com"},
		"aleg lcr/ in enterprise":     {"aleg": aleg + ":_:lcr/5145550199"},
		"bleg {originate_timeout}":    {"aleg": aleg, "bleg": "{originate_timeout=300}&park()"},
		"bleg [leg_timeout]":          {"aleg": aleg, "bleg": "[leg_timeout=300]1001"},
		"bleg space, not an app":      {"aleg": aleg, "bleg": "1001 XML ctx undef undef 300"},
		"bleg app plus extra args":    {"aleg": aleg, "bleg": "&park() undef undef undef undef 300"},
		"bleg quote":                  {"aleg": aleg, "bleg": "&transfer('1001 XML ctx')"},
		"bleg control":                {"aleg": aleg, "bleg": "&park()\r"},
		"dialplan space":              {"aleg": aleg, "dialplan": "XML 300"},
		"context space":               {"aleg": aleg, "context": "acme undef undef 300"},
		"context control":             {"aleg": aleg, "context": "acme\x00"},
		"caller_id_number space":      {"aleg": aleg, "caller_id_number": "514 555"},
		"caller_id_number comma":      {"aleg": aleg, "caller_id_number": "1,originate_timeout=300"},
		"caller_id_name quote":        {"aleg": aleg, "caller_id_name": "x' undef 300 '"},
		"caller_id_name comma":        {"aleg": aleg, "caller_id_name": "x,originate_timeout=300"},
		"value injects a key":         {"aleg": aleg, "channel_variables": map[string]any{"foo": "1,originate_timeout=300"}},
		"value with space":            {"aleg": aleg, "channel_variables": map[string]any{"foo": "a b"}},
		"key with =":                  {"aleg": aleg, "channel_variables": map[string]any{"originate_timeout=300,foo": "1"}},
		"value is an object":          {"aleg": aleg, "channel_variables": map[string]any{"foo": map[string]any{"a": 1}}},
	}
	for v := range originateTimingVars {
		cases["timing var "+v] = map[string]any{"aleg": aleg, "channel_variables": map[string]any{v: 300}}
		cases["timing var "+strings.ToUpper(v)] = map[string]any{"aleg": aleg, "channel_variables": map[string]any{strings.ToUpper(v): 300}}
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			fs := newFakeFreeSWITCH(t, "ClueCon-test", replyWith("+OK "+fxCallX+"\n"))
			h, es := originateHandlerFor(fs)
			rec := originate(t, h, fxUser1, fxDomainA, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if fs.acceptCount() != 0 || len(registeredCalls(es)) != 0 || len(h.originateSlots) != 0 {
				t.Fatalf("refused request reached FreeSWITCH (%d dials), registered or held a slot", fs.acceptCount())
			}
		})
	}
}

// Ordinary values stay accepted: forked and gateway dial strings, &app with
// and without arguments, a spaced caller ID name, plain channel variables.
func TestOriginateAcceptsOrdinaryInput(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"forked aleg":       {"aleg": "user/101@acme.example.com,user/102@acme.example.com"},
		"enterprise aleg":   {"aleg": "user/101@acme.example.com:_:user/102@acme.example.com"},
		"gateway aleg":      {"aleg": "sofia/gateway/carrier/+15145550199"},
		"plain bleg":        {"aleg": "user/101@acme.example.com", "bleg": "5145550199"},
		"app without args":  {"aleg": "user/101@acme.example.com", "bleg": "&echo"},
		"spaced cid name":   {"aleg": "user/101@acme.example.com", "caller_id_name": "Front Desk"},
		"channel variables": {"aleg": "user/101@acme.example.com", "channel_variables": map[string]any{"sip_h_X-Ticket": "T-42", "ignore_early_media": true, "hold_music": "local_stream://moh"}},
	} {
		t.Run(name, func(t *testing.T) {
			fs := newFakeFreeSWITCH(t, "ClueCon-test", replyWith("+OK "+fxCallX+"\n"))
			h, _ := originateHandlerFor(fs)
			if rec := originate(t, h, fxUser1, fxDomainA, body); rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if args := fsOriginateArgs(t, strings.TrimPrefix(fs.commands()[1], "1:api ")); len(args) != 7 || args[6] != "60" || !strings.HasPrefix(args[0], "{originate_timeout=60") {
				t.Fatalf("FreeSWITCH would see %d args %q", len(args), args)
			}
		})
	}
}
