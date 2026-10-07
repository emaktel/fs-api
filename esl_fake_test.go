package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
)

// fakeFreeSWITCH is a minimal ESL server on a real TCP socket: auth
// handshake, `event` subscriptions, `exit`, and api commands answered by a
// per-test handler.
type fakeFreeSWITCH struct {
	rep      *safeReporter
	ln       net.Listener
	password string

	// handleAPI answers one api command on connection connNo (1-based). It
	// writes zero or more frames to conn and returns true to close it.
	handleAPI func(connNo int, cmd string, conn net.Conn) bool

	mu      sync.Mutex
	accepts int
	cmds    []string
	// subscribed receives the connection number of each `event` subscription.
	subscribed chan int
	// closedByPeer receives the connection number when the client closes it.
	closedByPeer chan int
}

func newFakeFreeSWITCH(t *testing.T, password string, handleAPI func(connNo int, cmd string, conn net.Conn) bool) *fakeFreeSWITCH {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeFreeSWITCH{
		rep:          newSafeReporter(t),
		ln:           ln,
		password:     password,
		handleAPI:    handleAPI,
		subscribed:   make(chan int, 16),
		closedByPeer: make(chan int, 16),
	}
	t.Cleanup(func() { ln.Close() })
	go fs.acceptLoop()
	return fs
}

func (fs *fakeFreeSWITCH) hostPort() (string, string) {
	host, port, _ := net.SplitHostPort(fs.ln.Addr().String())
	return host, port
}

func (fs *fakeFreeSWITCH) acceptCount() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.accepts
}

func (fs *fakeFreeSWITCH) commands() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]string(nil), fs.cmds...)
}

func (fs *fakeFreeSWITCH) acceptLoop() {
	for {
		conn, err := fs.ln.Accept()
		if err != nil {
			return
		}
		fs.mu.Lock()
		fs.accepts++
		n := fs.accepts
		fs.mu.Unlock()
		go fs.serve(n, conn)
	}
}

func (fs *fakeFreeSWITCH) serve(connNo int, conn net.Conn) {
	defer conn.Close()
	if _, err := io.WriteString(conn, "Content-Type: auth/request\n\n"); err != nil {
		return
	}
	tp := textproto.NewReader(bufio.NewReader(conn))
	for {
		cmd, err := readCommand(tp)
		if err != nil {
			fs.closedByPeer <- connNo
			return
		}
		fs.mu.Lock()
		fs.cmds = append(fs.cmds, fmt.Sprintf("%d:%s", connNo, cmd))
		fs.mu.Unlock()
		switch {
		case strings.HasPrefix(cmd, "auth "):
			if strings.TrimPrefix(cmd, "auth ") != fs.password {
				io.WriteString(conn, "Content-Type: command/reply\nReply-Text: -ERR invalid\n\n")
				return
			}
			io.WriteString(conn, "Content-Type: command/reply\nReply-Text: +OK accepted\n\n")
		case strings.HasPrefix(cmd, "event "):
			io.WriteString(conn, "Content-Type: command/reply\nReply-Text: +OK event listener enabled plain\n\n")
			fs.subscribed <- connNo
		case cmd == "exit":
			io.WriteString(conn, "Content-Type: command/reply\nReply-Text: +OK bye\n\n")
			return
		case strings.HasPrefix(cmd, "api "):
			if fs.handleAPI(connNo, strings.TrimPrefix(cmd, "api "), conn) {
				return
			}
		default:
			fs.rep.Errorf("fake FreeSWITCH: unexpected command %q", cmd)
			return
		}
	}
}

// readCommand reads one ESL command: its first line, then lines up to the blank line.
func readCommand(tp *textproto.Reader) (string, error) {
	first, err := tp.ReadLine()
	if err != nil {
		return "", err
	}
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return "", err
		}
		if line == "" {
			return first, nil
		}
	}
}

func apiResponseFrame(body string) string {
	return fmt.Sprintf("Content-Type: api/response\nContent-Length: %d\n\n%s", len(body), body)
}

func disconnectNoticeFrame() string {
	body := "Disconnected, goodbye.\nSee you at ClueCon! http://www.cluecon.com/\n"
	return fmt.Sprintf("Content-Type: text/disconnect-notice\nContent-Length: %d\n\n%s", len(body), body)
}

func dumpJSON(callUUID, domainUUID string) string {
	m := fmt.Sprintf(`{"Event-Name":"CHANNEL_DATA","Unique-ID":%q,"Caller-Context":"public"`, callUUID)
	if domainUUID != "" {
		m += fmt.Sprintf(`,"variable_domain_uuid":%q`, domainUUID)
	}
	return m + "}"
}

// dumpTarget extracts the uuid from "uuid_dump <uuid> json" ("" for any other
// command, which then fails the reply's Unique-ID check).
func dumpTarget(cmd string) string {
	parts := strings.Fields(cmd)
	if len(parts) != 3 || parts[0] != "uuid_dump" || parts[2] != "json" {
		return ""
	}
	return parts[1]
}

// safeReporter forwards errors from helper goroutines to the test only while
// it is running; a goroutine that outlives its test must not fail it.
type safeReporter struct {
	t    *testing.T
	mu   sync.Mutex
	done bool
}

func newSafeReporter(t *testing.T) *safeReporter {
	r := &safeReporter{t: t}
	t.Cleanup(func() {
		r.mu.Lock()
		r.done = true
		r.mu.Unlock()
	})
	return r
}

func (r *safeReporter) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.done {
		r.t.Errorf(format, args...)
	}
}
