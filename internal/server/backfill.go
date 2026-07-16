package server

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strconv"
)

// Backfill serves the UI's scrollback: the browser only holds a bounded
// window of lines, and when the user scrolls above it, older lines are read
// straight from the file on disk — the hub's in-memory history stays small.
// The anchor is a byte offset (every streamed line carries its own), so
// pages are gap-free and never overlap.
const (
	defaultBackfillLines = 2000
	maxBackfillLines     = 5000
	backfillScanBlock    = 64 * 1024
	// maxBackfillScan bounds how far back one request will scan, so a file
	// full of enormous lines cannot balloon a response.
	maxBackfillScan = 16 << 20
	// maxBackfillLineBytes truncates pathological single lines for display.
	maxBackfillLineBytes = 256 * 1024
)

type backfillLine struct {
	Off  int64  `json:"off"`
	Text string `json:"text"`
}

type backfillResponse struct {
	File    string         `json:"file"`
	Lines   []backfillLine `json:"lines"`
	AtStart bool           `json:"atStart"`
}

// handleBefore returns up to limit complete lines that end strictly before
// byte offset "offset" in the given file, oldest first. Omitting offset (or
// passing a negative one) anchors at the end of the file. Only files this
// server is tailing may be read.
func (s *Server) handleBefore(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	file := q.Get("file")
	if !s.fileSet[file] {
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

	limit := defaultBackfillLines
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = min(n, maxBackfillLines)
	}

	lines, atStart, err := readLinesBefore(file, before, limit)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Tailed but not created yet: nothing before, by definition.
			writeJSON(w, backfillResponse{File: file, Lines: []backfillLine{}, AtStart: true})
			return
		}
		s.log.Warn("backfill failed", "file", file, "error", err)
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, backfillResponse{File: file, Lines: lines, AtStart: atStart})
}

// readLinesBefore reads backwards from byte offset before (or EOF when
// negative) and returns up to limit complete lines, oldest first. atStart
// reports that the first returned line is the first line of the file.
func readLinesBefore(path string, before int64, limit int) ([]backfillLine, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	end := info.Size()
	if before >= 0 && before < end {
		end = before
	}
	if end == 0 {
		return []backfillLine{}, true, nil
	}

	// Scan backwards in blocks until we have seen limit+1 newlines (the
	// extra one delimits the start of the oldest wanted line), hit the
	// start of the file, or exhaust the scan budget.
	var blocks [][]byte
	pos, newlines := end, 0
	for pos > 0 && newlines <= limit && end-pos < maxBackfillScan {
		n := int64(backfillScanBlock)
		if n > pos {
			n = pos
		}
		block := make([]byte, n)
		if _, err := f.ReadAt(block, pos-n); err != nil {
			return nil, false, fmt.Errorf("read at %d: %w", pos-n, err)
		}
		pos -= n
		newlines += bytes.Count(block, []byte{'\n'})
		blocks = append(blocks, block)
	}
	data := make([]byte, 0, end-pos)
	for i := len(blocks) - 1; i >= 0; i-- {
		data = append(data, blocks[i]...)
	}

	// data covers [pos, end). Unless we reached the start of the file, the
	// head is mid-line: drop through the first newline.
	start := pos
	if pos > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return []backfillLine{}, false, nil // one line larger than the scan budget
		}
		data = data[i+1:]
		start += int64(i + 1)
	}

	// The region now begins at a line start. Split into complete lines; a
	// trailing piece without a newline (possible only when anchored at a
	// file that does not end in one) is an unfinished line — skip it.
	var lines []backfillLine
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		text := bytes.TrimSuffix(data[:i], []byte{'\r'})
		if len(text) > maxBackfillLineBytes {
			text = text[:maxBackfillLineBytes]
		}
		lines = append(lines, backfillLine{Off: start, Text: string(text)})
		start += int64(i + 1)
		data = data[i+1:]
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	if lines == nil {
		lines = []backfillLine{}
	}
	atStart := len(lines) > 0 && lines[0].Off == 0
	return lines, atStart, nil
}
