package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fxCallX = "0d0d0d0d-0000-4000-8000-0000000000d1"
	fxCallY = "0d0d0d0d-0000-4000-8000-0000000000d2"
)

func newSerialClientFor(fs *fakeFreeSWITCH) *eslSerialClient {
	host, port := fs.hostPort()
	return newESLSerialClient(host, port, "ClueCon-test")
}

// dumpFor runs ChannelDump and returns the dump's variable_domain_uuid (to
// tell replies apart).
func dumpFor(t *testing.T, c *eslSerialClient, callUUID string, timeout time.Duration) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	dump, err := c.ChannelDump(ctx, callUUID)
	if err != nil {
		return "", err
	}
	if id := dump["Unique-ID"]; id != callUUID {
		t.Fatalf("dump for %v returned for %s", id, callUUID)
	}
	d, _ := dump["variable_domain_uuid"].(string)
	return d, nil
}

func TestESLSerialAuthAndHit(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, cmd string, conn net.Conn) bool {
		io.WriteString(conn, apiResponseFrame(dumpJSON(dumpTarget(cmd), fxDomainA)))
		return false
	})
	c := newSerialClientFor(fs)

	for i := 0; i < 2; i++ {
		got, err := dumpFor(t, c, fxCallX, time.Second)
		if err != nil || got != fxDomainA {
			t.Fatalf("dump %d = %q, %v; want %q", i, got, err, fxDomainA)
		}
	}
	if n := fs.acceptCount(); n != 1 {
		t.Fatalf("a healthy connection is reused: %d dials", n)
	}
	want := []string{"1:auth ClueCon-test", "1:api uuid_dump " + fxCallX + " json", "1:api uuid_dump " + fxCallX + " json"}
	if got := fs.commands(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestESLSerialAuthRefused(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "other-password", func(int, string, net.Conn) bool { return false })
	if _, err := dumpFor(t, newSerialClientFor(fs), fxCallX, time.Second); err == nil || !strings.Contains(err.Error(), "auth refused") {
		t.Fatalf("want auth refusal, got %v", err)
	}
}

// A reply for another call is rejected, without naming that call, and the
// connection is replaced.
func TestESLSerialRejectsReplyForAnotherCall(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(connNo int, cmd string, conn net.Conn) bool {
		target := dumpTarget(cmd)
		if connNo == 1 {
			target = fxCallY // swapped reply
		}
		io.WriteString(conn, apiResponseFrame(dumpJSON(target, fxDomainB)))
		return false
	})
	c := newSerialClientFor(fs)

	_, err := dumpFor(t, c, fxCallX, time.Second)
	if err == nil {
		t.Fatal("swapped reply accepted")
	}
	if strings.Contains(err.Error(), fxCallY) || strings.Contains(err.Error(), fxDomainB) {
		t.Fatalf("error reveals the other call: %v", err)
	}
	select {
	case n := <-fs.closedByPeer:
		if n != 1 {
			t.Fatalf("closed connection %d, want 1", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection not closed after a mismatched reply")
	}
	got, err := dumpFor(t, c, fxCallX, time.Second)
	if err != nil || got != fxDomainB {
		t.Fatalf("after redial: %q, %v", got, err)
	}
	if n := fs.acceptCount(); n != 2 {
		t.Fatalf("dials = %d, want 2", n)
	}
}

func TestESLSerialChannelGone(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, _ string, conn net.Conn) bool {
		io.WriteString(conn, apiResponseFrame("-ERR No such channel!\n"))
		return false
	})
	c := newSerialClientFor(fs)
	for i := 0; i < 2; i++ {
		if _, err := dumpFor(t, c, fxCallX, time.Second); !errors.Is(err, errChannelGone) {
			t.Fatalf("want errChannelGone, got %v", err)
		}
	}
	if n := fs.acceptCount(); n != 1 {
		t.Fatalf("a well-framed -ERR keeps the connection: dials = %d", n)
	}
}

// A command that times out closes its connection, so the late reply is never
// read by the next command (which asks about another call on a fresh connection).
func TestESLSerialTimeoutClosesAndLateReplyIsNeverRead(t *testing.T) {
	lateWritten := make(chan error, 1)
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(connNo int, cmd string, conn net.Conn) bool {
		target := dumpTarget(cmd)
		if connNo == 1 {
			go func() {
				time.Sleep(300 * time.Millisecond)
				_, err := io.WriteString(conn, apiResponseFrame(dumpJSON(target, fxDomainA)))
				lateWritten <- err
			}()
			return false
		}
		io.WriteString(conn, apiResponseFrame(dumpJSON(target, fxDomainB)))
		return false
	})
	c := newSerialClientFor(fs)

	if _, err := dumpFor(t, c, fxCallX, 100*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("want a timeout, got %v", err)
	}
	select {
	case <-fs.closedByPeer:
	case <-time.After(2 * time.Second):
		t.Fatal("timed-out connection was not closed")
	}
	<-lateWritten // the late reply has been sent to the dead connection

	got, err := dumpFor(t, c, fxCallY, time.Second)
	if err != nil || got != fxDomainB {
		t.Fatalf("next dump = %q, %v; want its own reply %q", got, err, fxDomainB)
	}
	if n := fs.acceptCount(); n != 2 {
		t.Fatalf("dials = %d, want 2", n)
	}
}

// Concurrent callers each get their own call's dump: the lock covers write and read.
func TestESLSerialConcurrentCallers(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, cmd string, conn net.Conn) bool {
		target := dumpTarget(cmd)
		domain := fxDomainA
		if target == fxCallY {
			domain = fxDomainB
		}
		io.WriteString(conn, apiResponseFrame(dumpJSON(target, domain)))
		return false
	})
	c := newSerialClientFor(fs)
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		for _, tc := range []struct{ call, domain string }{{fxCallX, fxDomainA}, {fxCallY, fxDomainB}} {
			go func() {
				got, err := dumpFor(t, c, tc.call, 2*time.Second)
				if err == nil && got != tc.domain {
					err = fmt.Errorf("call %s got domain %s", tc.call, got)
				}
				errs <- err
			}()
		}
	}
	for i := 0; i < 40; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := fs.acceptCount(); n != 1 {
		t.Fatalf("dials = %d, want 1", n)
	}
}

func TestESLSerialDisconnectNotice(t *testing.T) {
	t.Run("reply after the notice (linger) is read", func(t *testing.T) {
		fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, cmd string, conn net.Conn) bool {
			io.WriteString(conn, disconnectNoticeFrame()+apiResponseFrame(dumpJSON(dumpTarget(cmd), fxDomainA)))
			return false
		})
		if got, err := dumpFor(t, newSerialClientFor(fs), fxCallX, time.Second); err != nil || got != fxDomainA {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("notice then close fails the command and the next one redials", func(t *testing.T) {
		fs := newFakeFreeSWITCH(t, "ClueCon-test", func(connNo int, cmd string, conn net.Conn) bool {
			if connNo == 1 {
				io.WriteString(conn, disconnectNoticeFrame())
				return true
			}
			io.WriteString(conn, apiResponseFrame(dumpJSON(dumpTarget(cmd), fxDomainA)))
			return false
		})
		c := newSerialClientFor(fs)
		if _, err := dumpFor(t, c, fxCallX, time.Second); err == nil {
			t.Fatal("dump on a closing connection succeeded")
		}
		if got, err := dumpFor(t, c, fxCallX, time.Second); err != nil || got != fxDomainA {
			t.Fatalf("after redial: %q, %v", got, err)
		}
		if n := fs.acceptCount(); n != 2 {
			t.Fatalf("dials = %d, want 2", n)
		}
	})
}

// The serialized client fails closed on an oversized reply.
func TestESLSerialOversizedReplyFailsClosed(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, _ string, conn net.Conn) bool {
		io.WriteString(conn, apiResponseFrame(strings.Repeat("x", maxESLBody+1)))
		return false
	})
	c := newSerialClientFor(fs)
	if _, err := dumpFor(t, c, fxCallX, 2*time.Second); !errors.Is(err, errOversizedFrame) {
		t.Fatalf("want errOversizedFrame, got %v", err)
	}
	select {
	case <-fs.closedByPeer:
	case <-time.After(2 * time.Second):
		t.Fatal("connection not closed after an oversized reply")
	}
}

func TestESLSerialRefusesNonCanonicalCallUUID(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(int, string, net.Conn) bool { return false })
	if _, err := dumpFor(t, newSerialClientFor(fs), fxCallX+"\n\napi status", time.Second); err == nil {
		t.Fatal("non-canonical uuid dumped")
	}
	if n := fs.acceptCount(); n != 0 {
		t.Fatalf("dialed for a refused uuid: %d", n)
	}
}

func TestESLSerialUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	c := newESLSerialClient(host, port, "ClueCon-test")
	if _, err := dumpFor(t, c, fxCallX, time.Second); err == nil || !strings.Contains(err.Error(), "dial ESL") {
		t.Fatalf("want a dial error, got %v", err)
	}
}

// A caller whose deadline passed while it waited for the lock must not touch
// (and so must not close) the shared connection.
func TestESLSerialExpiredWaiterLeavesConnection(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, cmd string, conn net.Conn) bool {
		io.WriteString(conn, apiResponseFrame(dumpJSON(dumpTarget(cmd), fxDomainA)))
		return false
	})
	c := newSerialClientFor(fs)
	if got, err := dumpFor(t, c, fxCallX, time.Second); err != nil || got != fxDomainA {
		t.Fatalf("first dump: %q, %v", got, err)
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := c.ChannelDump(expired, fxCallX); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if got, err := dumpFor(t, c, fxCallX, time.Second); err != nil || got != fxDomainA {
		t.Fatalf("after the expired waiter: %q, %v", got, err)
	}
	if n := fs.acceptCount(); n != 1 {
		t.Fatalf("dials = %d, want 1", n)
	}
}

// Close (shutdown) closes the connection.
func TestESLSerialClose(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, cmd string, conn net.Conn) bool {
		io.WriteString(conn, apiResponseFrame(dumpJSON(dumpTarget(cmd), fxDomainA)))
		return false
	})
	c := newSerialClientFor(fs)
	if _, err := dumpFor(t, c, fxCallX, time.Second); err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case <-fs.closedByPeer:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not close the connection")
	}
}

func TestReadESLFrameOversizedBodyIsDiscarded(t *testing.T) {
	big := strings.Repeat("y", maxESLBody+10)
	stream := fmt.Sprintf("Content-Type: text/event-plain\nContent-Length: %d\n\n%s", len(big), big) + apiResponseFrame("UP\n")
	rd := bufio.NewReader(strings.NewReader(stream))
	hdr, _, err := readESLFrame(rd)
	if !errors.Is(err, errOversizedFrame) || hdr.Get("Content-Type") != "text/event-plain" {
		t.Fatalf("first frame: %v, %v", hdr, err)
	}
	hdr, body, err := readESLFrame(rd)
	if err != nil || hdr.Get("Content-Type") != "api/response" || string(body) != "UP\n" {
		t.Fatalf("frame after the oversized one: %v %q %v", hdr, body, err)
	}
}

// A reused connection that FreeSWITCH dropped while idle (its weekly
// restart) is redialed and the command retried once, transparently.
func TestESLSerialRetriesOnceOnStaleConnection(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(connNo int, cmd string, conn net.Conn) bool {
		io.WriteString(conn, apiResponseFrame(dumpJSON(dumpTarget(cmd), fxDomainA)))
		return connNo == 1 // connection 1 is dropped right after its first reply
	})
	c := newSerialClientFor(fs)
	if _, err := dumpFor(t, c, fxCallX, time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the close reach the client
	got, err := dumpFor(t, c, fxCallY, time.Second)
	if err != nil || got != fxDomainA {
		t.Fatalf("after the idle drop: %q, %v", got, err)
	}
	if n := fs.acceptCount(); n != 2 {
		t.Fatalf("dials = %d, want 2", n)
	}
}

// The retry happens at most once: if the fresh connection fails too, the
// command fails without a third dial.
func TestESLSerialRetriesAtMostOnce(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(connNo int, cmd string, conn net.Conn) bool {
		if connNo == 1 {
			io.WriteString(conn, apiResponseFrame(dumpJSON(dumpTarget(cmd), fxDomainA)))
		}
		return true // 1: dropped after its reply; 2: dropped without one
	})
	c := newSerialClientFor(fs)
	if _, err := dumpFor(t, c, fxCallX, time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := dumpFor(t, c, fxCallX, time.Second); err == nil {
		t.Fatal("succeeded without a reply")
	}
	if n := fs.acceptCount(); n != 2 {
		t.Fatalf("dials = %d, want 2", n)
	}
}

// A timeout on a reused connection is not retried (FreeSWITCH may still
// answer late; the connection is closed instead).
func TestESLSerialTimeoutOnReusedConnectionIsNotRetried(t *testing.T) {
	var answered sync.Once
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, cmd string, conn net.Conn) bool {
		answered.Do(func() { io.WriteString(conn, apiResponseFrame(dumpJSON(dumpTarget(cmd), fxDomainA))) })
		return false // later commands get no reply
	})
	c := newSerialClientFor(fs)
	if _, err := dumpFor(t, c, fxCallX, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := dumpFor(t, c, fxCallX, 100*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("want a timeout, got %v", err)
	}
	if n := fs.acceptCount(); n != 1 {
		t.Fatalf("dials = %d, want 1 (no retry)", n)
	}
}

// A graceful FreeSWITCH restart writes a disconnect-notice to the idle
// connection and closes it: the next command reads only the notice, then EOF,
// and is retried once on a fresh connection.
func TestESLSerialRetriesAfterDisconnectNoticeOnIdleConnection(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(connNo int, cmd string, conn net.Conn) bool {
		io.WriteString(conn, apiResponseFrame(dumpJSON(dumpTarget(cmd), fxDomainA)))
		if connNo == 1 {
			time.Sleep(20 * time.Millisecond) // the notice arrives while the connection is idle
			io.WriteString(conn, disconnectNoticeFrame())
			return true
		}
		return false
	})
	c := newSerialClientFor(fs)
	if _, err := dumpFor(t, c, fxCallX, time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	got, err := dumpFor(t, c, fxCallY, time.Second)
	if err != nil || got != fxDomainA {
		t.Fatalf("after the graceful restart: %q, %v", got, err)
	}
	if n := fs.acceptCount(); n != 2 {
		t.Fatalf("dials = %d, want 2", n)
	}
}

// Never retried once a byte of the api/response has arrived: a reply cut off
// mid-frame on a reused connection fails the command (no second dial).
func TestESLSerialNoRetryAfterPartialReply(t *testing.T) {
	var mu sync.Mutex
	cmds := 0
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, cmd string, conn net.Conn) bool {
		mu.Lock()
		cmds++
		n := cmds
		mu.Unlock()
		frame := apiResponseFrame(dumpJSON(dumpTarget(cmd), fxDomainA))
		if n == 1 {
			io.WriteString(conn, frame)
			return false
		}
		io.WriteString(conn, frame[:len(frame)-10]) // cut off, then closed
		return true
	})
	c := newSerialClientFor(fs)
	if _, err := dumpFor(t, c, fxCallX, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := dumpFor(t, c, fxCallX, time.Second); err == nil {
		t.Fatal("a cut-off reply succeeded")
	}
	if n := fs.acceptCount(); n != 1 {
		t.Fatalf("dials = %d, want 1 (no retry after reply bytes)", n)
	}
}
