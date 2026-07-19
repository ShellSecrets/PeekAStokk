// Package backfill reads older lines backwards from a log file on disk —
// the storage behind the UI's scrollback. It is shared by the server (for
// its own local files) and by the forwarding client (answering remote
// scrollback requests for files on its host), so both sides produce
// identical text, including Docker json-log unwrapping.
package backfill

import (
	"bytes"
	"fmt"
	"os"

	"github.com/shellsecrets/peekastokk/internal/dockerlog"
)

const (
	// DefaultLines is the page size when a request does not specify one.
	DefaultLines = 2000
	// MaxLines caps one request's page size.
	MaxLines  = 5000
	scanBlock = 64 * 1024
	// maxScan bounds how far back one request will scan, so a file full
	// of enormous lines cannot balloon a response.
	maxScan = 16 << 20
	// maxLineBytes truncates pathological single lines for display.
	maxLineBytes = 256 * 1024
)

// Line is one recovered line: its text and the byte offset of its first
// byte in the file (the anchor for further backwards pagination).
type Line struct {
	Off  int64  `json:"off"`
	Text string `json:"text"`
}

// ReadLinesBefore reads backwards from byte offset before (or EOF when
// negative) and returns up to limit complete lines, oldest first. atStart
// reports that the first returned line is the first line of the file.
func ReadLinesBefore(path string, before int64, limit int) (lines []Line, atStart bool, err error) {
	if limit < 1 {
		limit = DefaultLines
	}
	limit = min(limit, MaxLines)

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
		return []Line{}, true, nil
	}

	// Scan backwards in blocks until we have seen limit+1 newlines (the
	// extra one delimits the start of the oldest wanted line), hit the
	// start of the file, or exhaust the scan budget.
	var blocks [][]byte
	pos, newlines := end, 0
	for pos > 0 && newlines <= limit && end-pos < maxScan {
		n := min(int64(scanBlock), pos)
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
			return []Line{}, false, nil // one line larger than the scan budget
		}
		data = data[i+1:]
		start += int64(i + 1)
	}

	// The region now begins at a line start. Split into complete lines; a
	// trailing piece without a newline (possible only when anchored at a
	// file that does not end in one) is an unfinished line — skip it.
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		text := bytes.TrimSuffix(data[:i], []byte{'\r'})
		// Unwrap before the length cap: unwrapping only ever shrinks, and
		// this keeps scrollback text identical to the live tailer's.
		if unwrapped, ok := dockerlog.Unwrap(text); ok {
			text = unwrapped
		}
		if len(text) > maxLineBytes {
			text = text[:maxLineBytes]
		}
		lines = append(lines, Line{Off: start, Text: string(text)})
		start += int64(i + 1)
		data = data[i+1:]
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	if lines == nil {
		lines = []Line{}
	}
	atStart = len(lines) > 0 && lines[0].Off == 0
	return lines, atStart, nil
}
