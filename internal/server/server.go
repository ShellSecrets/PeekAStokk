// Package server exposes the web UI and the Server-Sent Events stream.
//
// SSE is used instead of WebSockets deliberately: log streaming is
// one-directional, EventSource reconnects automatically, and the
// Last-Event-ID header lets the server replay exactly the lines a client
// missed while disconnected — all with zero external dependencies.
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/shellsecrets/peekastokk/internal/auth"
	"github.com/shellsecrets/peekastokk/internal/hub"
)

//go:embed web/index.html
var indexHTML []byte

const (
	// subscriberBuffer is the per-client queue; a client that falls this
	// many events behind is evicted and reconnects via EventSource.
	subscriberBuffer = 1024
	// batchLimit bounds how many queued events are written before a flush,
	// so one slow write cannot delay the stream indefinitely.
	batchLimit        = 256
	heartbeatInterval = 15 * time.Second
)

// fileEntry is all the UI ever sees of a tailed file: an opaque id and a
// display name (the base name, deduplicated). The absolute path never
// leaves the server, so browsers — and anyone watching the wire — learn
// nothing about the host's directory layout.
type fileEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Options configure a Server.
type Options struct {
	// Files is the list of tailed paths.
	Files []string
	// Lines is the default number of lines the UI keeps on screen.
	Lines int
	// AuthUser/AuthPass enable HTTP basic authentication for everything
	// except /healthz. An empty AuthUser disables authentication. AuthPass
	// is either the plaintext password or an Argon2id hash in PHC format
	// (produced by peekastokk -hash-password).
	AuthUser string
	AuthPass string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Server serves the embedded UI, the event stream, and small JSON APIs.
type Server struct {
	hub      *hub.Hub
	entries  []fileEntry
	pathByID map[string]string // opaque id -> tailed path (server-side only)
	idByPath map[string]string
	lines    int
	authUser string
	authPass string // plaintext or argon2id PHC hash

	// verifiedPass caches the SHA-256 of the password that last passed the
	// deliberately slow Argon2id verification, so the KDF runs once per
	// process rather than on every request. Guarded by authMu; verifyMu
	// serializes the slow verifications themselves so a flood of wrong
	// passwords is bounded to one KDF at a time.
	authMu       sync.Mutex
	verifyMu     sync.Mutex
	verifiedPass [sha256.Size]byte
	verifiedSet  bool

	log *slog.Logger
	mux *http.ServeMux
}

// New builds a Server streaming from h.
func New(h *hub.Hub, opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Lines <= 0 {
		opts.Lines = 500
	}
	files := opts.Files
	names := displayNames(files)
	s := &Server{
		hub:      h,
		entries:  make([]fileEntry, len(files)),
		pathByID: make(map[string]string, len(files)),
		idByPath: make(map[string]string, len(files)),
		lines:    opts.Lines,
		authUser: opts.AuthUser,
		authPass: opts.AuthPass,
		log:      opts.Logger,
		mux:      http.NewServeMux(),
	}
	for i, path := range files {
		id := strconv.Itoa(i)
		s.entries[i] = fileEntry{ID: id, Name: names[i]}
		s.pathByID[id] = path
		s.idByPath[path] = id
	}
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /events", s.handleEvents)
	s.mux.HandleFunc("GET /api/files", s.handleFiles)
	s.mux.HandleFunc("GET /api/before", s.handleBefore)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	return s
}

// Handler returns the root handler for use with an http.Server, wrapped in
// basic authentication when credentials are configured.
func (s *Server) Handler() http.Handler {
	if s.authUser == "" {
		return s.mux
	}
	return s.requireBasicAuth(s.mux)
}

// requireBasicAuth guards every route except /healthz (load balancers need
// to probe unauthenticated). Credentials are compared in constant time via
// digests, so neither the comparison nor the length leaks timing.
func (s *Server) requireBasicAuth(next http.Handler) http.Handler {
	wantUser := sha256.Sum256([]byte(s.authUser))
	passIsHash := auth.IsHash(s.authPass)
	var wantPass [sha256.Size]byte
	if !passIsHash {
		wantPass = sha256.Sum256([]byte(s.authPass))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if user, pass, ok := r.BasicAuth(); ok {
			gotUser := sha256.Sum256([]byte(user))
			userOK := subtle.ConstantTimeCompare(gotUser[:], wantUser[:]) == 1
			var passOK bool
			if passIsHash {
				passOK = s.checkHashedPassword(pass)
			} else {
				gotPass := sha256.Sum256([]byte(pass))
				passOK = subtle.ConstantTimeCompare(gotPass[:], wantPass[:]) == 1
			}
			if userOK && passOK {
				next.ServeHTTP(w, r)
				return
			}
			s.log.Warn("rejected credentials", "remote", r.RemoteAddr, "user", user)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="PeekAStokk", charset="UTF-8"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	})
}

// checkHashedPassword verifies pass against the stored Argon2id hash. The
// digest of an accepted password is cached, so basic auth's
// credential-on-every-request pattern pays the slow KDF once per process.
func (s *Server) checkHashedPassword(pass string) bool {
	digest := sha256.Sum256([]byte(pass))

	s.authMu.Lock()
	cached, cachedSet := s.verifiedPass, s.verifiedSet
	s.authMu.Unlock()
	if cachedSet && subtle.ConstantTimeCompare(digest[:], cached[:]) == 1 {
		return true
	}

	s.verifyMu.Lock()
	ok, err := auth.VerifyPassword(pass, s.authPass)
	s.verifyMu.Unlock()
	if err != nil {
		s.log.Error("stored auth hash is invalid", "error", err)
		return false
	}
	if ok {
		s.authMu.Lock()
		s.verifiedPass, s.verifiedSet = digest, true
		s.authMu.Unlock()
	}
	return ok
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(indexHTML)
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"files": s.entries, "lines": s.lines})
}

// displayNames maps paths to their base names, numbering duplicates
// ("app.log #2") so two files from different directories stay
// distinguishable without revealing those directories.
func displayNames(paths []string) []string {
	total := make(map[string]int, len(paths))
	for _, p := range paths {
		total[filepath.Base(p)]++
	}
	seen := make(map[string]int, len(paths))
	names := make([]string, len(paths))
	for i, p := range paths {
		base := filepath.Base(p)
		if total[base] > 1 {
			seen[base]++
			names[i] = fmt.Sprintf("%s #%d", base, seen[base])
		} else {
			names[i] = base
		}
	}
	return names
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleEvents streams history and live events as SSE until the client
// disconnects, the hub closes, or the client is evicted for falling behind.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Replay only what the client has not seen. EventSource sends
	// Last-Event-ID on automatic reconnects; the UI passes ?after= when it
	// deliberately reconnects with a different file selection. Take the
	// newer of the two.
	var afterSeq uint64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			afterSeq = n
		}
	}
	if v := r.URL.Query().Get("after"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > afterSeq {
			afterSeq = n
		}
	}

	// An optional files filter (repeated ?files= values, opaque ids)
	// restricts the stream to selected files, so unselected files cost
	// the client nothing. Absent means every file.
	var fileFilter []string
	if vals, ok := r.URL.Query()["files"]; ok {
		for _, v := range vals {
			if v == "" {
				continue
			}
			path, known := s.pathByID[v]
			if !known {
				http.Error(w, "unknown file in files filter", http.StatusBadRequest)
				return
			}
			fileFilter = append(fileFilter, path)
		}
		if len(fileFilter) == 0 {
			http.Error(w, "files filter is empty", http.StatusBadRequest)
			return
		}
	}

	sub, history := s.hub.Subscribe(subscriberBuffer, afterSeq, fileFilter)
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // disable buffering in nginx-style proxies
	w.WriteHeader(http.StatusOK)

	s.log.Debug("sse client connected", "remote", r.RemoteAddr, "after_seq", afterSeq)
	defer s.log.Debug("sse client disconnected", "remote", r.RemoteAddr)

	for _, ev := range history {
		if s.writeEvent(w, ev, true) != nil {
			return
		}
	}
	flusher.Flush()

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-sub.Events():
			if !open || !s.writeBatch(w, flusher, sub, ev) {
				return
			}
		case <-heartbeat.C:
			// Comment line: keeps intermediaries from idling out the
			// connection and lets us detect a gone client.
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeBatch writes first plus any events already queued (up to batchLimit)
// and flushes once. It reports whether streaming should continue.
func (s *Server) writeBatch(w io.Writer, flusher http.Flusher, sub *hub.Subscriber, first hub.Event) bool {
	if s.writeEvent(w, first, false) != nil {
		return false
	}
	for i := 0; i < batchLimit; i++ {
		select {
		case ev, open := <-sub.Events():
			if !open || s.writeEvent(w, ev, false) != nil {
				flusher.Flush()
				return false
			}
		default:
			flusher.Flush()
			return true
		}
	}
	flusher.Flush()
	return true
}

// writeEvent serializes ev for the wire, replacing the internal file path
// with its opaque id. replay marks events the client did not observe live
// (history sent on connect/reconnect); the UI shows those without a
// timestamp, since the recorded time is when the server read the line, not
// when it happened.
func (s *Server) writeEvent(w io.Writer, ev hub.Event, replay bool) error {
	wire := struct {
		Seq    uint64    `json:"seq"`
		File   string    `json:"file"` // opaque id, never the path
		Text   string    `json:"text"`
		Off    int64     `json:"off"`
		Time   time.Time `json:"time"`
		Replay bool      `json:"replay,omitempty"`
	}{ev.Seq, s.idByPath[ev.File], ev.Text, ev.Off, ev.Time, replay}
	data, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	// json.Marshal escapes newlines, so data is always a single SSE line.
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, data)
	return err
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}
