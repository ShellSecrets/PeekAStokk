// Package server exposes the web UI and the Server-Sent Events stream.
//
// SSE is used instead of WebSockets deliberately: log streaming is
// one-directional, EventSource reconnects automatically, and the
// Last-Event-ID header lets the server replay exactly the lines a client
// missed while disconnected — all with zero external dependencies.
package server

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

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

// Server serves the embedded UI, the event stream, and small JSON APIs.
type Server struct {
	hub     *hub.Hub
	files   []string
	fileSet map[string]bool // exact tailed paths /api/before may read
	lines   int
	log     *slog.Logger
	mux     *http.ServeMux
}

// New builds a Server streaming from h; files is the list of tailed paths
// shown in the UI and lines is the default number of lines the UI keeps on
// screen (the user can change it there).
func New(h *hub.Hub, files []string, lines int, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if lines <= 0 {
		lines = 500
	}
	fileSet := make(map[string]bool, len(files))
	for _, f := range files {
		fileSet[f] = true
	}
	s := &Server{hub: h, files: files, fileSet: fileSet, lines: lines, log: logger, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /events", s.handleEvents)
	s.mux.HandleFunc("GET /api/files", s.handleFiles)
	s.mux.HandleFunc("GET /api/before", s.handleBefore)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	return s
}

// Handler returns the root handler for use with an http.Server.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(indexHTML)
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"files": s.files, "lines": s.lines})
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

	// EventSource sends Last-Event-ID on reconnect; replay only what the
	// client has not seen.
	var afterSeq uint64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			afterSeq = n
		}
	}

	sub, history := s.hub.Subscribe(subscriberBuffer, afterSeq)
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // disable buffering in nginx-style proxies
	w.WriteHeader(http.StatusOK)

	s.log.Debug("sse client connected", "remote", r.RemoteAddr, "after_seq", afterSeq)
	defer s.log.Debug("sse client disconnected", "remote", r.RemoteAddr)

	for _, ev := range history {
		if writeEvent(w, ev) != nil {
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
			if !open || !writeBatch(w, flusher, sub, ev) {
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
func writeBatch(w io.Writer, flusher http.Flusher, sub *hub.Subscriber, first hub.Event) bool {
	if writeEvent(w, first) != nil {
		return false
	}
	for i := 0; i < batchLimit; i++ {
		select {
		case ev, open := <-sub.Events():
			if !open || writeEvent(w, ev) != nil {
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

func writeEvent(w io.Writer, ev hub.Event) error {
	data, err := json.Marshal(ev)
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
