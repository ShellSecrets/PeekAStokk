// Package tail implements a polling file tailer.
//
// Polling (rather than fsnotify-style watching) keeps the package free of
// platform-specific dependencies and works on filesystems that do not
// deliver change notifications (NFS, SMB, some container mounts). The tailer
// survives the two common log-rotation strategies:
//
//   - rename + recreate: the path is re-stat'ed every poll and compared with
//     os.SameFile; on mismatch the old handle is drained and the new file is
//     read from the beginning.
//   - copytruncate: a shrinking size is treated as truncation and reading
//     restarts at offset zero.
//
// A file that does not exist yet is waited for, so tailing can be started
// before the producing process.
package tail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"time"
)

// Line is a single log line read from a tailed file.
type Line struct {
	File   string    // the path as given to New
	Text   string    // line content without the trailing newline (or \r\n)
	Offset int64     // byte offset of the line's first byte in the file
	Time   time.Time // when the line was read
}

// Options configure a Tailer.
type Options struct {
	// PollInterval is how often the file is checked for new data.
	// Zero or negative selects the default (200ms).
	PollInterval time.Duration

	// TailBytes bounds how much existing content is replayed when the file
	// is first opened: at most the trailing TailBytes bytes, aligned to the
	// next line boundary. Zero selects the default (64 KiB); a negative
	// value disables replay and starts at the end of the file.
	TailBytes int64

	// MaxLineBytes bounds the size of a single emitted line. Longer lines
	// are split into chunks of this size so a file without newlines cannot
	// grow memory without bound. Zero or negative selects the default (256 KiB).
	MaxLineBytes int

	// Logger receives warnings about transient read failures.
	// Nil selects slog.Default().
	Logger *slog.Logger
}

const (
	defaultPollInterval = 200 * time.Millisecond
	defaultTailBytes    = 64 * 1024
	defaultMaxLineBytes = 256 * 1024
	readChunkBytes      = 64 * 1024
)

// Tailer follows a single file and emits its lines. It is not safe for
// concurrent use; each Tailer is driven by exactly one Run call.
type Tailer struct {
	path string
	opts Options
	log  *slog.Logger

	file         *os.File
	fileInfo     os.FileInfo
	offset       int64
	pending      []byte // bytes after the last newline seen so far
	pendingStart int64  // file offset of pending[0]
	skipFirst    bool   // drop bytes up to the first newline (opened mid-line)
	openedOnce   bool
	readBuf      []byte
	lastWarn     string // dedupes repeated poll warnings
}

// New returns a Tailer for path. The file does not need to exist yet.
func New(path string, opts Options) *Tailer {
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	if opts.TailBytes == 0 {
		opts.TailBytes = defaultTailBytes
	}
	if opts.MaxLineBytes <= 0 {
		opts.MaxLineBytes = defaultMaxLineBytes
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Tailer{
		path:    path,
		opts:    opts,
		log:     opts.Logger,
		readBuf: make([]byte, readChunkBytes),
	}
}

// Run tails the file until ctx is cancelled, sending each complete line to
// out. Transient failures (file missing, permission errors, read errors) are
// logged and retried on the next poll; the only returned error is ctx.Err().
func (t *Tailer) Run(ctx context.Context, out chan<- Line) error {
	ticker := time.NewTicker(t.opts.PollInterval)
	defer ticker.Stop()
	defer t.closeFile()

	for {
		if err := t.poll(ctx, out); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			t.warnOnce(err)
			t.closeFile() // reopen from scratch on the next poll
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// poll performs one observation cycle: open the file if needed, detect
// rotation or truncation, and read any new data.
func (t *Tailer) poll(ctx context.Context, out chan<- Line) error {
	if t.file == nil {
		if err := t.open(); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // wait for the file to appear
			}
			return err
		}
	}

	info, statErr := os.Stat(t.path)
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", t.path, statErr)
	}

	rotated := statErr != nil || !os.SameFile(info, t.fileInfo)
	if rotated {
		// Drain what is left of the old file, including a trailing
		// unterminated line, then reopen on the next poll.
		if err := t.read(ctx, out); err != nil {
			return err
		}
		if err := t.flushPending(ctx, out); err != nil {
			return err
		}
		t.closeFile()
		return nil
	}

	if info.Size() < t.offset {
		// Truncated in place (e.g. copytruncate rotation): start over.
		if _, err := t.file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek %s: %w", t.path, err)
		}
		t.offset = 0
		t.pending = t.pending[:0]
		t.pendingStart = 0
		t.skipFirst = false
	}

	return t.read(ctx, out)
}

func (t *Tailer) open() error {
	f, err := os.Open(t.path)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat %s: %w", t.path, err)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return fmt.Errorf("%s: not a regular file", t.path)
	}

	t.file, t.fileInfo, t.offset = f, info, 0
	t.pending = t.pending[:0]
	t.skipFirst = false
	t.lastWarn = ""

	// Only the very first open replays a bounded amount of existing
	// content; after a rotation the new file is read in full.
	if !t.openedOnce {
		t.openedOnce = true
		start := int64(0)
		if t.opts.TailBytes < 0 {
			start = info.Size()
		} else if info.Size() > t.opts.TailBytes {
			start = info.Size() - t.opts.TailBytes
		}
		if start > 0 {
			if _, err := f.Seek(start, io.SeekStart); err != nil {
				t.closeFile()
				return fmt.Errorf("seek %s: %w", t.path, err)
			}
			t.offset = start
			// Unless the previous byte is a newline we landed mid-line, so
			// everything up to the next newline must be discarded.
			prev := make([]byte, 1)
			_, err := f.ReadAt(prev, start-1)
			t.skipFirst = err != nil || prev[0] != '\n'
		}
	}
	t.pendingStart = t.offset
	return nil
}

// read consumes everything between the current offset and EOF.
func (t *Tailer) read(ctx context.Context, out chan<- Line) error {
	for {
		n, err := t.file.Read(t.readBuf)
		if n > 0 {
			dataStart := t.offset
			t.offset += int64(n)
			if cerr := t.consume(ctx, out, t.readBuf[:n], dataStart); cerr != nil {
				return cerr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read %s: %w", t.path, err)
		}
	}
}

// consume splits data (whose first byte sits at file offset dataStart) into
// lines, buffering any trailing partial line.
func (t *Tailer) consume(ctx context.Context, out chan<- Line, data []byte, dataStart int64) error {
	if t.skipFirst {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return nil // still inside the first (partial) line
		}
		data = data[i+1:]
		dataStart += int64(i + 1)
		t.skipFirst = false
	}

	buf, cur := data, dataStart
	if len(t.pending) > 0 {
		t.pending = append(t.pending, data...)
		buf, cur = t.pending, t.pendingStart
	}
	max := t.opts.MaxLineBytes
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSuffix(buf[:i], []byte{'\r'})
		off := cur
		for len(line) > max {
			if err := t.emit(ctx, out, line[:max], off); err != nil {
				return err
			}
			line = line[max:]
			off += int64(max)
		}
		if err := t.emit(ctx, out, line, off); err != nil {
			return err
		}
		cur += int64(i + 1)
		buf = buf[i+1:]
	}
	// Cap the partial-line buffer so a file without newlines cannot grow
	// memory without bound.
	for len(buf) > max {
		if err := t.emit(ctx, out, buf[:max], cur); err != nil {
			return err
		}
		buf = buf[max:]
		cur += int64(max)
	}
	t.pending = append(t.pending[:0], buf...)
	t.pendingStart = cur
	return nil
}

func (t *Tailer) flushPending(ctx context.Context, out chan<- Line) error {
	if len(t.pending) == 0 {
		return nil
	}
	err := t.emit(ctx, out, bytes.TrimSuffix(t.pending, []byte{'\r'}), t.pendingStart)
	t.pending = t.pending[:0]
	return err
}

// emit sends b as-is; callers strip any trailing \r first so that chunks of
// an oversized split line are never altered mid-line.
func (t *Tailer) emit(ctx context.Context, out chan<- Line, b []byte, off int64) error {
	select {
	case out <- Line{File: t.path, Text: string(b), Offset: off, Time: time.Now()}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *Tailer) closeFile() {
	if t.file != nil {
		t.file.Close()
		t.file, t.fileInfo = nil, nil
	}
}

// warnOnce logs err, suppressing consecutive duplicates so a persistent
// failure does not emit one warning per poll interval.
func (t *Tailer) warnOnce(err error) {
	if msg := err.Error(); msg != t.lastWarn {
		t.lastWarn = msg
		t.log.Warn("tail poll failed", "file", t.path, "error", err)
	}
}
