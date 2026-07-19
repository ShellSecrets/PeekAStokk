// Package forward implements the client half of PeekAStokk's log
// forwarding: it pushes tailed lines to a receiving server's /ingest
// endpoint over one long-lived, full-duplex HTTP request (NDJSON lines
// out, NDJSON acks back), reconnecting with backoff and buffering a
// bounded number of lines across disconnects.
package forward

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/shellsecrets/peekastokk/internal/ingestproto"
)

const (
	// DefaultBufferLines bounds the in-memory retry buffer; once full the
	// oldest line is overwritten, mirroring the hub's ring-buffer
	// convention (bounded history, newest wins).
	DefaultBufferLines = 5000
	// ackTimeout declares a connection dead when no ack (the server sends
	// one every second) arrives for this long.
	ackTimeout = 30 * time.Second
	// authBackoff is the flat retry delay after a 401/403: retrying a
	// permanently wrong credential at network-blip speed is useless spam.
	authBackoff = 30 * time.Second
	// stableAfter resets the exponential backoff once a connection has
	// stayed up this long.
	stableAfter = 10 * time.Second
)

// Options configure a Client.
type Options struct {
	// BufferLines bounds the retry buffer; zero selects DefaultBufferLines.
	BufferLines int
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// HTTPClient defaults to a client with no overall timeout (the
	// connection is deliberately long-lived).
	HTTPClient *http.Client
}

// Status is a point-in-time snapshot for monitoring.
type Status struct {
	Connected       bool      `json:"connected"`
	LastError       string    `json:"last_error,omitempty"`
	LastConnectedAt time.Time `json:"last_connected_at,omitzero"`
	BufferedLines   int       `json:"buffered_lines"`
	LinesSent       uint64    `json:"lines_sent"`
	DroppedLines    uint64    `json:"dropped_lines"`
}

type entry struct {
	seq  uint64
	line ingestproto.Line
}

// Client forwards enqueued lines to one server. Enqueue is safe for
// concurrent use; Run must be called exactly once.
type Client struct {
	url   string // ".../ingest"
	token string
	log   *slog.Logger
	httpc *http.Client
	max   int

	mu        sync.Mutex
	buf       []entry // ordered by seq, un-acked
	nextSeq   uint64
	dropped   uint64
	sent      uint64
	dropping  bool // are we currently in an overflow episode (for log dedup)
	connected bool
	lastErr   error
	lastConn  time.Time
	wake      chan struct{}
}

// New builds a Client pushing to serverURL's /ingest endpoint,
// authenticating with token.
func New(serverURL, token string, opts Options) *Client {
	if opts.BufferLines <= 0 {
		opts.BufferLines = DefaultBufferLines
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{}
	}
	return &Client{
		url:   serverURL + "/ingest",
		token: token,
		log:   opts.Logger,
		httpc: opts.HTTPClient,
		max:   opts.BufferLines,
		wake:  make(chan struct{}, 1),
	}
}

// Enqueue buffers one line for delivery. When the buffer is full the
// oldest un-acked line is dropped; every transition into an overflow
// episode is logged once, with a running counter in Status.
func (c *Client) Enqueue(source, text string, off int64, ts time.Time) {
	c.mu.Lock()
	c.nextSeq++
	if len(c.buf) >= c.max {
		c.buf = c.buf[1:]
		c.dropped++
		if !c.dropping {
			c.dropping = true
			c.log.Warn("forward buffer full; dropping oldest lines",
				"capacity", c.max, "dropped_total", c.dropped)
		}
	}
	c.buf = append(c.buf, entry{
		seq:  c.nextSeq,
		line: ingestproto.Line{Seq: c.nextSeq, Source: source, Text: text, Off: off, Time: ts},
	})
	c.mu.Unlock()

	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Status returns a snapshot of the client's state.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{
		Connected:       c.connected,
		LastConnectedAt: c.lastConn,
		BufferedLines:   len(c.buf),
		LinesSent:       c.sent,
		DroppedLines:    c.dropped,
	}
	if c.lastErr != nil {
		st.LastError = c.lastErr.Error()
	}
	return st
}

// errAuthRejected marks a 401/403 from the server, which gets the flat
// (not exponential) retry delay.
var errAuthRejected = errors.New("server rejected the forward token")

// Run connects, pumps, and reconnects until ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	attempt := 0
	for {
		start := time.Now()
		err := c.connectAndPump(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		c.mu.Lock()
		c.connected = false
		c.lastErr = err
		c.mu.Unlock()

		var delay time.Duration
		if errors.Is(err, errAuthRejected) {
			delay = authBackoff
			c.log.Error("ingest token rejected — check forward-token against the server's ingest= entry",
				"server", c.url, "retry_in", delay)
			attempt = 0
		} else {
			if time.Since(start) >= stableAfter {
				attempt = 0 // the previous connection was healthy; a blip, not a pattern
			}
			delay = Backoff(attempt)
			attempt++
			c.log.Warn("forward connection lost; reconnecting",
				"server", c.url, "error", err, "retry_in", delay.Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// preflight verifies the token with a cheap bodyless GET before the
// streaming POST is opened. A 401 on the stream itself would arrive while
// the request body (an unwritten pipe) is still pending, which wedges the
// transport; checking auth on a normal request avoids that entirely.
func (c *Client) preflight(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return errAuthRejected
	default:
		return fmt.Errorf("preflight: server returned %s", resp.Status)
	}
}

// connectAndPump runs one connection to completion: it streams buffered
// and newly enqueued lines into the request body while reading acks off
// the response body, trimming the buffer as acks confirm receipt.
func (c *Client) connectAndPump(ctx context.Context) error {
	if err := c.preflight(ctx); err != nil {
		return err
	}

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	pr, pw := io.Pipe()
	defer pw.Close()

	req, err := http.NewRequestWithContext(connCtx, http.MethodPost, c.url, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/x-ndjson")

	// Guard the connection phase: a server (or intermediary) that answers
	// without reading our body would otherwise leave Do blocked forever.
	connectTimer := time.AfterFunc(15*time.Second, cancel)
	resp, err := c.httpc.Do(req)
	connectTimer.Stop()
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errAuthRejected
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", resp.Status)
	}

	c.mu.Lock()
	c.connected = true
	c.lastConn = time.Now()
	c.lastErr = nil
	c.mu.Unlock()
	c.log.Info("forward connection established", "server", c.url)

	// Watchdog: the server acks every second; prolonged silence means the
	// connection is dead even if TCP has not noticed yet.
	watchdog := time.AfterFunc(ackTimeout, cancel)
	defer watchdog.Stop()

	// Ack reader: trims the buffer. Sole reader of the response body.
	ackErr := make(chan error, 1)
	go func() {
		dec := json.NewDecoder(resp.Body)
		for {
			var ack ingestproto.Ack
			if err := dec.Decode(&ack); err != nil {
				ackErr <- err
				return
			}
			watchdog.Reset(ackTimeout)
			c.mu.Lock()
			i := 0
			for i < len(c.buf) && c.buf[i].seq <= ack.Ack {
				i++
			}
			c.buf = c.buf[i:]
			if c.dropping && len(c.buf) < c.max {
				c.dropping = false
			}
			c.mu.Unlock()
		}
	}()

	// Writer: everything in the buffer not yet sent on THIS connection.
	// A new connection resends the whole buffer (at-least-once delivery;
	// the server-side UI dedups nothing, but acked lines were trimmed, so
	// only genuinely unconfirmed lines repeat).
	enc := json.NewEncoder(pw)
	var sentUpTo uint64
	for {
		c.mu.Lock()
		var pending []ingestproto.Line
		for _, e := range c.buf {
			if e.seq > sentUpTo {
				pending = append(pending, e.line)
				sentUpTo = e.seq
			}
		}
		c.mu.Unlock()

		for _, ln := range pending {
			if err := enc.Encode(ln); err != nil {
				return fmt.Errorf("writing line: %w", err)
			}
		}
		if n := len(pending); n > 0 {
			c.mu.Lock()
			c.sent += uint64(n)
			c.mu.Unlock()
		}

		select {
		case <-connCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("connection timed out (no acks)")
		case err := <-ackErr:
			return fmt.Errorf("ack stream ended: %w", err)
		case <-c.wake:
		}
	}
}
