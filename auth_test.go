package main

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

type fakeDumper struct {
	dump  map[string]any
	err   error
	calls int
}

func (f *fakeDumper) ChannelDump(context.Context, string) (map[string]any, error) {
	f.calls++
	return f.dump, f.err
}

// getCallContext authorizes call operations from a uuid_dump. On the
// serialized client a reply for another call is refused and never decides
// the requested call's context, nor is it revealed to the API caller.
func TestGetCallContextRefusesReplyForAnotherCall(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, _ string, conn net.Conn) bool {
		io.WriteString(conn, apiResponseFrame(`{"Unique-ID":"`+fxCallY+`","variable_accountcode":"other.example.com"}`))
		return false
	})
	h := &APIHandler{channelDumps: newSerialClientFor(fs)}
	info, err := h.getCallContext(fxCallX)
	if err == nil || info != nil {
		t.Fatalf("want a mismatch error, got %+v, %v", info, err)
	}
	if strings.Contains(err.Error(), fxCallY) || strings.Contains(err.Error(), "other.example.com") {
		t.Fatalf("error leaks the other call: %v", err)
	}
}

func TestGetCallContextThroughSerialClient(t *testing.T) {
	fs := newFakeFreeSWITCH(t, "ClueCon-test", func(_ int, cmd string, conn net.Conn) bool {
		io.WriteString(conn, apiResponseFrame(`{"Unique-ID":"`+dumpTarget(cmd)+`","variable_accountcode":"acme.example.com"}`))
		return false
	})
	h := &APIHandler{channelDumps: newSerialClientFor(fs)}
	info, err := h.getCallContext(fxCallX)
	if err != nil || !info.Found || info.AccountCode != "acme.example.com" {
		t.Fatalf("got %+v, %v", info, err)
	}
}

func TestGetCallContextNotFound(t *testing.T) {
	h := &APIHandler{channelDumps: &fakeDumper{err: errChannelGone}}
	info, err := h.getCallContext(fxCallX)
	if err != nil || info.Found {
		t.Fatalf("got %+v, %v", info, err)
	}
}

func TestGetCallContextNonCanonicalUUIDIsNotFound(t *testing.T) {
	d := &fakeDumper{err: errors.New("must not be called")}
	h := &APIHandler{channelDumps: d}
	info, err := h.getCallContext(strings.ToUpper(fxCallX))
	if err != nil || info.Found || d.calls != 0 {
		t.Fatalf("got %+v, %v (dumps %d)", info, err, d.calls)
	}
}

func TestGetCallContextTransportErrorFails(t *testing.T) {
	h := &APIHandler{channelDumps: &fakeDumper{err: errors.New("i/o timeout")}}
	if info, err := h.getCallContext(fxCallX); err == nil {
		t.Fatalf("want an error, got %+v", info)
	}
}
