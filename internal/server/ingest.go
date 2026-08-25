package server

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shellsecrets/peekastokk/internal/auth"
	"github.com/shellsecrets/peekastokk/internal/backfill"
	"github.com/shellsecrets/peekastokk/internal/ingestproto"
)

const (
	// maxSourcesPerClient caps how many distinct sources one forwarding
	// client may register, so a misbehaving client cannot grow the
	// registry unboundedly — same spirit as main's maxTailedFiles.
	maxSourcesPerClient = 500
	// ackInterval is how often the server reports the highest received
	// Seq back to the client; short enough that the client's bounded
	// retry buffer trims promptly under load.
	ackInterval = 1 * time.Second
)

// remoteBackfillTimeout bounds how long /api/before waits for a forwarding
// client to answer a scrollback request. A var so tests can shorten it.
var remoteBackfillTimeout = 10 * time.Second

// ingestConn is one live forwarding connection, registered so remote
// scrollback requests can be routed to it.
type ingestConn struct {
	client string
	reqCh  chan ingestproto.BackfillReq
}

func (s *Server) addConn(c *ingestConn) {
	s.connMu.Lock()
	s.conns[c.client] = c
	s.connMu.Unlock()
}

func (s *Server) removeConn(c *ingestConn) {
	s.connMu.Lock()
	if s.conns[c.client] == c { // a newer connection may have replaced us
		delete(s.conns, c.client)
	}
	s.connMu.Unlock()
}

// requestRemoteBackfill relays a scrollback read to the named client over
// its live ingest connection and assembles the chunked response. ok is
// false when the client is offline, times out, reports an error, or the
// request cannot be delivered — callers degrade to "no further history".
func (s *Server) requestRemoteBackfill(r *http.Request, clientName, source string, before int64, limit int) ([]backfill.Line, bool, bool) {
	s.connMu.Lock()
	conn := s.conns[clientName]
	s.connMu.Unlock()
	if conn == nil {
		return nil, false, false
	}

	id := s.reqID.Add(1)
	ch := make(chan ingestproto.BackfillResp, 8)
	s.pendMu.Lock()
	s.pending[id] = ch
	s.pendMu.Unlock()
	defer func() {
		s.pendMu.Lock()
		delete(s.pending, id)
		s.pendMu.Unlock()
	}()

	select {
	case conn.reqCh <- ingestproto.BackfillReq{ID: id, Source: source, Before: before, Limit: limit}:
	default:
		return nil, false, false // connection's request queue is saturated
	}

	timeout := time.After(remoteBackfillTimeout)
	lines := []backfill.Line{}
	atStart := false
	for {
		select {
		case <-r.Context().Done():
			return nil, false, false
		case <-timeout:
			s.log.Warn("remote backfill timed out", "client", clientName, "source", source)
			return nil, false, false
		case resp := <-ch:
			if resp.Err != "" {
				s.log.Debug("remote backfill refused", "client", clientName, "source", source, "error", resp.Err)
				return nil, false, false
			}
			for _, ln := range resp.Lines {
				lines = append(lines, backfill.Line{Off: ln.Off, Text: ln.Text})
			}
			if len(lines) > backfill.MaxLines {
				lines = lines[len(lines)-backfill.MaxLines:]
			}
			if resp.AtStart {
				atStart = true
			}
			if resp.Final {
				return lines, atStart, true
			}
		}
	}
}

// routeBackfillResp delivers one response chunk to whoever is waiting on
// its request id; chunks for forgotten requests (timed out, browser gone)
// are dropped.
func (s *Server) routeBackfillResp(resp *ingestproto.BackfillResp) {
	s.pendMu.Lock()
	ch := s.pending[resp.ID]
	s.pendMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- *resp:
	default: // waiter stopped draining; it will time out on its own
	}
}

// ingestCred is one configured "ingest = name:token-or-hash" identity.
// The verified-token digest is cached per credential so an Argon2id hash
// is verified at most once per process per token, mirroring the viewer
// auth's checkHashedPassword pattern.
type ingestCred struct {
	name   string
	secret string // plaintext token or argon2id PHC hash

	mu       sync.Mutex
	verified [sha256.Size]byte
	cached   bool
}

// check reports whether token matches this credential.
func (c *ingestCred) check(token string) bool {
	digest := sha256.Sum256([]byte(token))

	c.mu.Lock()
	cached, ok := c.verified, c.cached
	c.mu.Unlock()
	if ok && subtle.ConstantTimeCompare(digest[:], cached[:]) == 1 {
		return true
	}

	var match bool
	if auth.IsHash(c.secret) {
		got, err := auth.VerifyPassword(token, c.secret)
		match = err == nil && got
	} else {
		want := sha256.Sum256([]byte(c.secret))
		match = subtle.ConstantTimeCompare(digest[:], want[:]) == 1
	}
	if match {
		c.mu.Lock()
		c.verified, c.cached = digest, true
		c.mu.Unlock()
	}
	return match
}

// checkIngestToken resolves a bearer token to the client name it
// authenticates as.
func (s *Server) checkIngestToken(token string) (string, bool) {
	for _, c := range s.ingestCreds {
		if c.check(token) {
			return c.name, true
		}
	}
	return "", false
}

// forwardKey is the registry key for a forwarded source. It can never
// collide with a local file's key: local keys are filesystem paths.
func forwardKey(clientName, source string) string {
	return "forward:" + clientName + "/" + source
}

// handleIngestCheck is the auth preflight: forwarding clients verify their
// token with a cheap GET before opening the streaming POST, because an
// early 401 on a request whose body is an unwritten pipe would otherwise
// leave the client's transport wedged mid-request.
func (s *Server) handleIngestCheck(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="PeekAStokk ingest"`)
		http.Error(w, "bearer token required", http.StatusUnauthorized)
		return
	}
	if _, ok := s.checkIngestToken(token); !ok {
		s.rejectIngest(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rejectIngest logs, applies the failed-auth delay, and responds 401.
func (s *Server) rejectIngest(w http.ResponseWriter, r *http.Request) {
	s.log.Warn("rejected ingest token", "remote", r.RemoteAddr)
	select {
	case <-time.After(failedAuthDelay):
	case <-r.Context().Done():
	}
	// The bearer challenge tells a rejected client (or whoever is
	// debugging one) that the token reached the ingest route and was
	// refused there, rather than being stopped by the UI's basic auth or
	// a proxy in front of it.
	w.Header().Set("WWW-Authenticate", `Bearer realm="PeekAStokk ingest"`)
	http.Error(w, "invalid token", http.StatusUnauthorized)
}

// handleIngest receives one forwarding client's line stream. The request
// body carries NDJSON ingestproto.Lines; the response body carries
// periodic ingestproto.Acks — the mirror image of /events' heartbeat,
// over one full-duplex HTTP request.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="PeekAStokk ingest"`)
		http.Error(w, "bearer token required", http.StatusUnauthorized)
		return
	}
	clientName, ok := s.checkIngestToken(token)
	if !ok {
		s.rejectIngest(w, r)
		return
	}

	rc := http.NewResponseController(w)
	if err := rc.EnableFullDuplex(); err != nil {
		// HTTP/2 is full-duplex already; on HTTP/1.x this should not
		// fail with the stdlib server.
		s.log.Debug("EnableFullDuplex unsupported", "error", err)
	}
	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	s.log.Info("ingest client connected", "client", clientName, "remote", r.RemoteAddr)
	defer s.log.Info("ingest client disconnected", "client", clientName, "remote", r.RemoteAddr)

	// Register for remote scrollback routing.
	conn := &ingestConn{client: clientName, reqCh: make(chan ingestproto.BackfillReq, 16)}
	s.addConn(conn)
	defer s.removeConn(conn)

	// The down-writer goroutine is the sole writer to w after the header
	// (acks on a ticker, plus relayed scrollback requests); the main
	// goroutine only reads the body. The handler must not return until
	// the goroutine has fully exited, or a final write could race the
	// server's own response teardown.
	var highestSeq atomic.Uint64
	ackDone := make(chan struct{})
	ackExited := make(chan struct{})
	defer func() {
		close(ackDone)
		<-ackExited
	}()
	go func() {
		defer close(ackExited)
		ticker := time.NewTicker(ackInterval)
		defer ticker.Stop()
		enc := json.NewEncoder(w)
		for {
			select {
			case <-ackDone:
				return
			case <-r.Context().Done():
				return
			case req := <-conn.reqCh:
				if enc.Encode(ingestproto.Ack{Ack: highestSeq.Load(), Req: &req, Time: time.Now()}) != nil {
					return
				}
				rc.Flush()
			case <-ticker.C:
				if enc.Encode(ingestproto.Ack{Ack: highestSeq.Load(), Time: time.Now()}) != nil {
					return
				}
				rc.Flush()
			}
		}
	}()

	// sources this connection has been allowed to publish under; capped
	// via the registry so reconnecting with fresh names cannot grow it
	// unboundedly.
	allowed := make(map[string]string) // source -> registry key
	capWarned := false

	sc := bufio.NewScanner(r.Body)
	sc.Buffer(make([]byte, 64*1024), ingestproto.MaxLineBytes)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var up ingestproto.Up
		if err := json.Unmarshal(raw, &up); err != nil {
			// One corrupt line must not kill an otherwise healthy stream.
			s.log.Debug("skipping malformed ingest line", "client", clientName, "error", err)
			continue
		}
		if up.Resp != nil {
			s.routeBackfillResp(up.Resp)
			continue
		}
		ln := up.Line
		if ln.Source == "" {
			continue
		}
		key, ok := allowed[ln.Source]
		if !ok {
			key = forwardKey(clientName, ln.Source)
			if _, exists := s.reg.lookupID(key); !exists && s.reg.countForwarded(clientName) >= maxSourcesPerClient {
				if !capWarned {
					capWarned = true
					s.log.Warn("ingest client exceeded source cap; ignoring further new sources",
						"client", clientName, "cap", maxSourcesPerClient)
				}
				continue
			}
			s.reg.registerForwarded(clientName, key, clientName+"/"+ln.Source)
			allowed[ln.Source] = key
		}
		ts := ln.Time
		if ts.IsZero() {
			ts = time.Now()
		}
		if !s.hub.Publish(key, ln.Text, ln.Off, ts) {
			// Hub closed (server shutting down): end the connection
			// WITHOUT acking this line, so the client keeps it buffered
			// and redelivers to the next server instance.
			s.log.Debug("hub closed; ending ingest stream unacked", "client", clientName)
			return
		}
		if ln.Seq > highestSeq.Load() {
			highestSeq.Store(ln.Seq)
		}
	}
	if err := sc.Err(); err != nil && r.Context().Err() == nil {
		s.log.Debug("ingest stream ended", "client", clientName, "error", err)
	}
}

// registerForwarded records a forwarded source, tracking the per-client
// count for the source cap.
func (rg *registry) registerForwarded(clientName, key, baseName string) {
	if _, isNew := rg.register(key, baseName, entryForwarded); isNew {
		rg.mu.Lock()
		rg.forwardCounts[clientName]++
		rg.mu.Unlock()
	}
}

// countForwarded reports how many distinct sources clientName has
// registered so far.
func (rg *registry) countForwarded(clientName string) int {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	return rg.forwardCounts[clientName]
}
