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
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var errChannelGone = errors.New("channel no longer exists")

// errOversizedFrame: a frame body over maxESLBody. readESLFrame has already
// read and discarded the body, so the stream stays in sync.
var errOversizedFrame = errors.New("ESL frame body over the size cap")

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// maxESLBody bounds one ESL frame body kept in memory (a uuid_dump is a few KB).
const maxESLBody = 1 << 20

// eslSerialClient is an ESL client on its own connection that runs one
// command at a time, for the commands whose reply decides who a call belongs
// to (`uuid_dump` for call authorization and call details).
//
// eslgo does not match replies to requests (any waiter takes the next reply),
// and a timed-out caller leaves its late reply for the next one. Here one
// mutex is held across each write and its read, every command runs under a
// deadline, and on any failure the connection is closed and redialed lazily
// by the next command, so a late reply can never be read as another's.
type eslSerialClient struct {
	addr     string
	password string

	mu   sync.Mutex
	conn *eslConn
}

// eslConn is one authenticated ESL connection. in counts the bytes read from
// the socket, so a failed command can tell whether any reply arrived.
type eslConn struct {
	net.Conn
	rd *bufio.Reader
	in *countingReader
}

// consumed is the number of bytes handed to readers so far (read from the
// socket minus what still sits in the buffer).
func (c *eslConn) consumed() int64 { return c.in.n - int64(c.rd.Buffered()) }

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// staleConnError: a command failed on a connection before any byte of its
// api/response arrived, by a write error or the connection being closed (not
// a timeout), with at most text/disconnect-notice frames read before:
// typically a connection FreeSWITCH dropped while it sat idle, abruptly or
// gracefully (a restart sends a disconnect-notice, then closes).
type staleConnError struct{ err error }

func (e *staleConnError) Error() string { return e.err.Error() }
func (e *staleConnError) Unwrap() error { return e.err }

func newESLSerialClient(host, port, password string) *eslSerialClient {
	return &eslSerialClient{addr: net.JoinHostPort(host, port), password: password}
}

// ChannelDump runs `uuid_dump <uuid> json` and returns the dump only if it is
// for callUUID; errChannelGone when FreeSWITCH has no such channel.
func (c *eslSerialClient) ChannelDump(ctx context.Context, callUUID string) (map[string]any, error) {
	if !canonicalUUID.MatchString(callUUID) {
		return nil, fmt.Errorf("refusing to dump non-canonical call uuid %q", callUUID)
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("uuid_dump without a deadline")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// A caller that waited out its deadline for the lock leaves the connection alone.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cmd := "uuid_dump " + callUUID + " json"
	reused := c.conn != nil
	body, err := c.apiLocked(ctx, cmd)
	var stale *staleConnError
	if err != nil && reused && errors.As(err, &stale) && ctx.Err() == nil {
		// A reused connection that FreeSWITCH dropped while idle (e.g. its
		// weekly restart): redial and retry once. The Unique-ID check still
		// applies to the retried reply.
		log.Printf("[ESL] Serialized connection was stale (%v); redialing once", err)
		c.closeLocked()
		body, err = c.apiLocked(ctx, cmd)
	}
	if err != nil {
		c.closeLocked()
		return nil, err
	}
	dump, err := parseChannelDump(body, callUUID)
	if err != nil && !errors.Is(err, errChannelGone) {
		// A reply we can't account for means the stream can't be trusted.
		c.closeLocked()
	}
	return dump, err
}

// Close closes the connection (shutdown); a later command would redial.
func (c *eslSerialClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

// apiLocked sends one api command on the connection, dialing it first if
// needed. c.mu must be held.
func (c *eslSerialClient) apiLocked(ctx context.Context, cmd string) ([]byte, error) {
	if c.conn == nil {
		deadline, _ := ctx.Deadline()
		conn, err := dialESL(ctx, c.addr, c.password, deadline)
		if err != nil {
			return nil, err
		}
		c.conn = conn
		log.Printf("[ESL] Serialized command connection established")
	}
	return sendAPI(ctx, c.conn, cmd)
}

// eslOneShot runs each api command on its own short-lived connection: dial,
// authenticate, send, read exactly one api/response, close. The reply can
// only be this command's. Used for originate, which can block for the whole
// ring time and so must not hold a shared connection.
type eslOneShot struct {
	addr     string
	password string
}

func newESLOneShot(host, port, password string) eslOneShot {
	return eslOneShot{addr: net.JoinHostPort(host, port), password: password}
}

// API runs cmd (without the "api " prefix). ctx must carry a deadline that
// covers the whole command.
func (o eslOneShot) API(ctx context.Context, cmd string) ([]byte, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("ESL command without a deadline")
	}
	conn, err := dialESL(ctx, o.addr, o.password, deadline)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return sendAPI(ctx, conn, cmd)
}

// sendAPI writes one api command and reads exactly its api/response, under
// ctx's deadline. A failure before any byte of the api/response arrived
// (other than a timeout) is a *staleConnError.
func sendAPI(ctx context.Context, conn *eslConn, cmd string) ([]byte, error) {
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	// Cancellation (shutdown) interrupts a blocked read as well as the deadline.
	// If it fires, wait for it before returning, so it can never land on the
	// next command's deadline.
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Unix(1, 0))
		close(fired)
	})
	defer func() {
		if !stop() {
			<-fired
		}
	}()

	if _, err := io.WriteString(conn, "api "+cmd+"\n\n"); err != nil {
		err = fmt.Errorf("write %s: %w", cmd, err)
		if !isTimeout(err) {
			return nil, &staleConnError{err}
		}
		return nil, err
	}
	// onlyNotices: every frame read so far for this command was a
	// disconnect-notice, so nothing of a reply has arrived yet.
	onlyNotices := true
	for {
		frameStart := conn.consumed()
		hdr, body, err := readESLFrame(conn.rd)
		if err != nil {
			err = fmt.Errorf("read reply to %s: %w", cmd, err)
			if onlyNotices && conn.consumed() == frameStart && !isTimeout(err) && isConnClosed(err) {
				return nil, &staleConnError{err}
			}
			return nil, err
		}
		if hdr.Get("Content-Type") != "text/disconnect-notice" {
			onlyNotices = false
		}
		switch ct := hdr.Get("Content-Type"); ct {
		case "api/response":
			return body, nil
		case "text/disconnect-notice":
			// FreeSWITCH is closing this connection. With linger the reply can
			// still follow; otherwise the next read fails and the command errors.
			continue
		default:
			return nil, fmt.Errorf("unexpected ESL frame %q in reply to %s", ct, cmd)
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// isConnClosed: the peer closed or reset the connection.
func isConnClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func (c *eslSerialClient) closeLocked() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// parseChannelDump accepts a `uuid_dump <uuid> json` reply only if it is for callUUID.
func parseChannelDump(body []byte, callUUID string) (map[string]any, error) {
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("-ERR")) {
		if bytes.Contains(trimmed, []byte("No such channel")) {
			return nil, errChannelGone
		}
		return nil, fmt.Errorf("uuid_dump %s: %s", callUUID, trimmed)
	}
	var dump map[string]any
	if err := json.Unmarshal(trimmed, &dump); err != nil {
		return nil, fmt.Errorf("uuid_dump %s: unreadable reply: %w", callUUID, err)
	}
	if id, _ := dump["Unique-ID"].(string); id != callUUID {
		// The other call's id is logged here only, never returned to a caller.
		log.Printf("[ESL] uuid_dump reply for call %q answered the request for %q; refusing it", id, callUUID)
		return nil, fmt.Errorf("uuid_dump reply did not match call %s", callUUID)
	}
	return dump, nil
}

// dialESL opens an inbound ESL connection and authenticates, all before
// deadline. The connection's deadline is left at deadline.
func dialESL(ctx context.Context, addr, password string, deadline time.Time) (*eslConn, error) {
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial ESL: %w", err)
	}
	if err := raw.SetDeadline(deadline); err != nil {
		raw.Close()
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	in := &countingReader{r: raw}
	conn := &eslConn{Conn: raw, rd: bufio.NewReader(in), in: in}

	hdr, _, err := readESLFrame(conn.rd)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("read ESL greeting: %w", err)
	}
	if ct := hdr.Get("Content-Type"); ct != "auth/request" {
		raw.Close()
		return nil, fmt.Errorf("unexpected ESL greeting %q", ct)
	}
	if err := eslCommand(conn, "auth "+password); err != nil {
		raw.Close()
		return nil, fmt.Errorf("ESL auth refused: %w", err)
	}
	return conn, nil
}

// eslCommand writes a command and requires a `command/reply` of +OK.
func eslCommand(conn *eslConn, cmd string) error {
	if _, err := io.WriteString(conn, cmd+"\n\n"); err != nil {
		return err
	}
	hdr, _, err := readESLFrame(conn.rd)
	if err != nil {
		return err
	}
	if hdr.Get("Content-Type") != "command/reply" || !strings.HasPrefix(hdr.Get("Reply-Text"), "+OK") {
		return fmt.Errorf("reply %q", hdr.Get("Reply-Text"))
	}
	return nil
}

// readESLFrame reads one ESL frame: MIME-style headers, a blank line, then
// exactly Content-Length bytes of body. A body over maxESLBody is read and
// discarded and errOversizedFrame is returned with the headers, so a caller
// can skip the frame and keep reading.
func readESLFrame(rd *bufio.Reader) (textproto.MIMEHeader, []byte, error) {
	hdr, err := textproto.NewReader(rd).ReadMIMEHeader()
	if err != nil {
		return nil, nil, err
	}
	cl := hdr.Get("Content-Length")
	if cl == "" {
		return hdr, nil, nil
	}
	n, err := strconv.ParseInt(cl, 10, 64)
	if err != nil || n < 0 {
		return nil, nil, fmt.Errorf("bad Content-Length %q", cl)
	}
	if n > maxESLBody {
		if _, err := io.CopyN(io.Discard, rd, n); err != nil {
			return nil, nil, err
		}
		return hdr, nil, fmt.Errorf("%w (%d bytes)", errOversizedFrame, n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(rd, body); err != nil {
		return nil, nil, err
	}
	return hdr, body, nil
}
