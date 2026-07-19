package server

import (
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/shellsecrets/peekastokk/internal/backfill"
)

// Scrollback: the browser only holds a bounded window of lines; when the
// user scrolls above it, older lines are read from the file on disk — the
// hub's in-memory history stays small. For a local file the server reads
// its own disk; for a forwarded source it relays the request to the
// owning client over the live ingest connection, which reads *its* disk.
// The anchor is a byte offset (every streamed line carries its own, in
// the coordinates of the file it came from), so pages are gap-free and
// never overlap either way.

type backfillResponse struct {
	File    string          `json:"file"`
	Lines   []backfill.Line `json:"lines"`
	AtStart bool            `json:"atStart"`
}

// handleBefore returns up to limit complete lines that end strictly before
// byte offset "offset" in the given source, oldest first. Omitting offset
// (or passing a negative one) anchors at the end. The source is named by
// its opaque id; only sources this server knows may be read.
func (s *Server) handleBefore(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	file := q.Get("file")
	path, kind, known := s.reg.lookupPath(file)
	if !known {
		http.Error(w, "unknown file", http.StatusNotFound)
		return
	}

	before := int64(-1)
	if v := q.Get("offset"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			http.Error(w, "invalid offset", http.StatusBadRequest)
			return
		}
		before = n
	}

	limit := backfill.DefaultLines
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = min(n, backfill.MaxLines)
	}

	empty := backfillResponse{File: file, Lines: []backfill.Line{}, AtStart: true}

	if kind == entryForwarded {
		// The file lives on the forwarding client's disk: relay the read
		// over its live ingest connection. A client that is offline, slow,
		// or too old to understand the request degrades to "no further
		// history" — exactly the pre-relay behavior.
		clientName, source, ok := parseForwardKey(path)
		if !ok {
			writeJSON(w, empty)
			return
		}
		lines, atStart, ok := s.requestRemoteBackfill(r, clientName, source, before, limit)
		if !ok {
			writeJSON(w, empty)
			return
		}
		writeJSON(w, backfillResponse{File: file, Lines: lines, AtStart: atStart})
		return
	}

	lines, atStart, err := backfill.ReadLinesBefore(path, before, limit)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Tailed but not created yet: nothing before, by definition.
			writeJSON(w, empty)
			return
		}
		s.log.Warn("backfill failed", "file", path, "error", err)
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, backfillResponse{File: file, Lines: lines, AtStart: atStart})
}

// parseForwardKey splits a "forward:<client>/<source>" registry key.
// Client names cannot contain '/' (config-validated), so the first slash
// is the separator; the source part is opaque and may contain anything.
func parseForwardKey(key string) (clientName, source string, ok bool) {
	rest, found := strings.CutPrefix(key, "forward:")
	if !found {
		return "", "", false
	}
	clientName, source, found = strings.Cut(rest, "/")
	if !found || clientName == "" || source == "" {
		return "", "", false
	}
	return clientName, source, true
}
