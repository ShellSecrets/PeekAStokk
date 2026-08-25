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

	"github.com/shellsecrets/peekastokk/internal/backfill"
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
	// ResolvePath maps a source display name back to the local file path
	// it is tailed from, for answering the server's remote scrollback
	// requests. Nil disables remote scrollback (requests are refused).
	// Only sources this client itself configured ever resolve — the
	// server cannot request arbitrary paths.
	ResolvePath func(source string) (string, bool)
	// Sources lists every source currently tailed, announced on each
	// connect so the server knows about quiet files without waiting for
	// them to produce a line. Nil skips the announcement.
	Sources func() []string
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
	url     string // ".../ingest"
	token   string
	log     *slog.Logger
	httpc   *http.Client
	max     int
	resolve func(source string) (string, bool)
	sources func() []string

	// respMu guards the queue of pending backfill-response chunks, sent
	// interleaved with lines on the same NDJSON stream.
	respMu    sync.Mutex
	respQueue []ingestproto.BackfillResp

	// The retry buffer is a fixed ring (ring[head] is the oldest of count
	// entries): no slice-shift reallocation churn at steady-state overflow
	// (was ~355 amortized B/op from periodic whole-buffer copies with the
	// previous buf=buf[1:] scheme; now 0 B/op — BenchmarkEnqueueSteadyOverflow),
	// and released slots are zeroed so dropped/acked line text is freed
	// immediately instead of lingering in the backing array.
	mu        sync.Mutex
	ring      []entry
	head      int
	count     int
	nextSeq   uint64
	dropped   uint64
	sent      uint64
	dropping  bool // are we currently in an overflow episode (for log dedup)
	announce  bool // re-announce the source list on the live connection
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
		url:     serverURL + "/ingest",
		token:   token,
		log:     opts.Logger,
		httpc:   opts.HTTPClient,
		max:     opts.BufferLines,
		resolve: opts.ResolvePath,
		sources: opts.Sources,
		ring:    make([]entry, opts.BufferLines),
		wake:    make(chan struct{}, 1),
	}
}

// handleBackfillReq answers one remote scrollback request: resolve the
// source to a local file, read backwards from disk, and queue the result
// as chunks small enough to stay under the wire's per-message size cap.
func (c *Client) handleBackfillReq(req ingestproto.BackfillReq) {
	refuse := func(reason string) {
		c.queueResp(ingestproto.BackfillResp{ID: req.ID, Final: true, Err: reason})
	}
	if c.resolve == nil {
		refuse("remote scrollback disabled")
		return
	}
	path, ok := c.resolve(req.Source)
	if !ok {
		refuse("unknown source")
		return
	}
	lines, atStart, err := backfill.ReadLinesBefore(path, req.Before, req.Limit)
	if err != nil {
		c.log.Debug("backfill read failed", "source", req.Source, "error", err)
		refuse("read failed")
		return
	}

	// Chunk by cumulative text size so one NDJSON message never
	// approaches ingestproto.MaxLineBytes even for maximum-width lines.
	const chunkBudget = ingestproto.MaxLineBytes / 4
	chunk := ingestproto.BackfillResp{ID: req.ID}
	budget := chunkBudget
	for _, ln := range lines {
		if budget-len(ln.Text) < 0 && len(chunk.Lines) > 0 {
			c.queueResp(chunk)
			chunk = ingestproto.BackfillResp{ID: req.ID}
			budget = chunkBudget
		}
		chunk.Lines = append(chunk.Lines, ingestproto.BackfillLine{Off: ln.Off, Text: ln.Text})
		budget -= len(ln.Text) + 32
	}
	chunk.AtStart = atStart
	chunk.Final = true
	c.queueResp(chunk)
}

func (c *Client) queueResp(resp ingestproto.BackfillResp) {
	c.respMu.Lock()
	c.respQueue = append(c.respQueue, resp)
	c.respMu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// takeResps drains the queued response chunks.
func (c *Client) takeResps() []ingestproto.BackfillResp {
	c.respMu.Lock()
	out := c.respQueue
	c.respQueue = nil
	c.respMu.Unlock()
	return out
}

// Enqueue buffers one line for delivery. When the buffer is full the
// oldest un-acked line is dropped; every transition into an overflow
// episode is logged once, with a running counter in Status.
func (c *Client) Enqueue(source, text string, off int64, ts time.Time) {
	c.mu.Lock()
	c.nextSeq++
	if c.count == c.max {
		c.ring[c.head] = entry{} // release the dropped line's text now
		c.head = (c.head + 1) % c.max
		c.count--
		c.dropped++
		if !c.dropping {
			c.dropping = true
			c.log.Warn("forward buffer full; dropping oldest lines",
				"capacity", c.max, "dropped_total", c.dropped)
		}
	}
	c.ring[(c.head+c.count)%c.max] = entry{
		seq:  c.nextSeq,
		line: ingestproto.Line{Seq: c.nextSeq, Source: source, Text: text, Off: off, Time: ts},
	}
	c.count++
	c.mu.Unlock()

	c.wakeWriter()
}

// SourcesChanged tells the client its set of tailed sources changed (a
// container started, a watched directory gained or lost a file), so the
// current list is announced again on the live connection. Safe for
// concurrent use; a no-op while disconnected, since connecting announces
// the list anyway.
func (c *Client) SourcesChanged() {
	if c.sources == nil {
		return
	}
	c.mu.Lock()
	c.announce = true
	c.mu.Unlock()
	c.wakeWriter()
}

// wakeWriter nudges the connection's writer loop without blocking.
func (c *Client) wakeWriter() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// trimAcked drops every buffered entry with seq <= ack, zeroing the freed
// slots so their line text is collectible immediately.
func (c *Client) trimAcked(ack uint64) {
	c.mu.Lock()
	for c.count > 0 && c.ring[c.head].seq <= ack {
		c.ring[c.head] = entry{}
		c.head = (c.head + 1) % c.max
		c.count--
	}
	if c.dropping && c.count < c.max {
		c.dropping = false
	}
	c.mu.Unlock()
}

// Status returns a snapshot of the client's state.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{
		Connected:       c.connected,
		LastConnectedAt: c.lastConn,
		BufferedLines:   c.count,
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

// announceSources sends the current source list, if the caller supplied a
// way to enumerate it.
func (c *Client) announceSources(enc *json.Encoder) error {
	if c.sources == nil {
		return nil
	}
	sources := c.sources()
	if len(sources) == 0 {
		return nil
	}
	if err := enc.Encode(ingestproto.Announce{Sources: sources}); err != nil {
		return fmt.Errorf("announcing sources: %w", err)
	}
	return nil
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

	// Ack reader: trims the buffer and dispatches remote scrollback
	// requests. Sole reader of the response body. Backfill reads run in
	// their own goroutine so a slow disk cannot stall ack processing;
	// concurrency is bounded by the server's per-connection request queue.
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
			c.trimAcked(ack.Ack)
			if ack.Req != nil {
				go c.handleBackfillReq(*ack.Req)
			}
		}
	}()

	// Writer: everything in the buffer not yet sent on THIS connection.
	// A new connection resends the whole buffer (at-least-once delivery;
	// the server-side UI dedups nothing, but acked lines were trimmed, so
	// only genuinely unconfirmed lines repeat).
	enc := json.NewEncoder(pw)
	// Announce the current sources first: a server that restarted (or is
	// seeing this client for the first time) then lists every tailed
	// file right away, including the quiet ones that would otherwise stay
	// invisible until they happened to be written to.
	if err := c.announceSources(enc); err != nil {
		return err
	}
	var sentUpTo uint64
	var pending []ingestproto.Line // reused across wakeups
	for {
		c.mu.Lock()
		reannounce := c.announce
		c.announce = false
		c.mu.Unlock()
		if reannounce {
			if err := c.announceSources(enc); err != nil {
				return err
			}
		}
		c.mu.Lock()
		pending = pending[:0]
		for i := 0; i < c.count; i++ {
			e := c.ring[(c.head+i)%c.max]
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
		// Backfill response chunks share the stream, wrapped so the
		// server (and, harmlessly, an older server) can tell them apart
		// from lines.
		for _, resp := range c.takeResps() {
			if err := enc.Encode(struct {
				Resp *ingestproto.BackfillResp `json:"resp"`
			}{&resp}); err != nil {
				return fmt.Errorf("writing backfill response: %w", err)
			}
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
