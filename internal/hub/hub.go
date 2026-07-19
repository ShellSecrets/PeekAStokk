// Package hub fans log events out to any number of subscribers and keeps a
// bounded history so late-joining subscribers can catch up.
package hub

import (
	"sync"
	"time"
)

// Event is a log line with a hub-assigned, monotonically increasing
// sequence number. Sequence numbers double as SSE event IDs, letting a
// reconnecting client resume where it left off.
type Event struct {
	Seq  uint64    `json:"seq"`
	File string    `json:"file"`
	Text string    `json:"text"`
	Off  int64     `json:"off"` // byte offset of the line's start in its file
	Time time.Time `json:"time"`
}

// Hub is safe for concurrent use.
type Hub struct {
	mu      sync.Mutex
	nextSeq uint64
	ring    []Event // fixed-capacity history, ring[start] is the oldest
	start   int
	count   int
	subs    map[*Subscriber]struct{}
	evicted uint64
	closed  bool
}

// New returns a Hub that retains the most recent historySize events.
func New(historySize int) *Hub {
	if historySize < 0 {
		historySize = 0
	}
	return &Hub{
		ring: make([]Event, historySize),
		subs: make(map[*Subscriber]struct{}),
	}
}

// Publish records one line in the history and delivers it to every
// subscriber, reporting whether the hub accepted it (false once closed —
// callers relaying for a remote source must not acknowledge such lines).
// A subscriber whose buffer is full is evicted (its channel is closed)
// rather than allowed to stall the producers; an evicted SSE client
// simply reconnects and resumes from its last event ID.
func (h *Hub) Publish(file, text string, off int64, ts time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}

	h.nextSeq++
	ev := Event{Seq: h.nextSeq, File: file, Text: text, Off: off, Time: ts}

	if n := len(h.ring); n > 0 {
		h.ring[(h.start+h.count)%n] = ev
		if h.count < n {
			h.count++
		} else {
			h.start = (h.start + 1) % n
		}
	}

	for s := range h.subs {
		if !s.wants(ev.File) {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			delete(h.subs, s)
			s.closed = true
			close(s.ch)
			h.evicted++
		}
	}
	return true
}

// Subscribe registers a new subscriber whose channel holds up to buffer
// undelivered events, and returns the retained history with sequence
// numbers greater than afterSeq (pass 0 for the full history). A non-empty
// files list restricts both history and delivery to those file paths; nil
// or empty means every file. Events published after Subscribe returns are
// delivered on the channel, so the history snapshot and the stream are
// gap-free. If the hub is already closed, the returned subscriber's channel
// is closed.
func (h *Hub) Subscribe(buffer int, afterSeq uint64, files []string) (*Subscriber, []Event) {
	if buffer <= 0 {
		buffer = 64
	}
	var want map[string]bool
	if len(files) > 0 {
		want = make(map[string]bool, len(files))
		for _, f := range files {
			want[f] = true
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	history := make([]Event, 0, h.count)
	for i := 0; i < h.count; i++ {
		ev := h.ring[(h.start+i)%len(h.ring)]
		if ev.Seq > afterSeq && (want == nil || want[ev.File]) {
			history = append(history, ev)
		}
	}

	s := &Subscriber{hub: h, ch: make(chan Event, buffer), files: want}
	if h.closed {
		s.closed = true
		close(s.ch)
	} else {
		h.subs[s] = struct{}{}
	}
	return s, history
}

// Close evicts all subscribers and makes further Publish calls no-ops.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for s := range h.subs {
		s.closed = true
		close(s.ch)
	}
	h.subs = make(map[*Subscriber]struct{})
}

// Subscriber receives published events on Events until it is closed,
// evicted, or the hub shuts down.
type Subscriber struct {
	hub    *Hub
	ch     chan Event
	files  map[string]bool // nil means every file
	closed bool            // guarded by hub.mu
}

func (s *Subscriber) wants(file string) bool { return s.files == nil || s.files[file] }

// Events returns the delivery channel. It is closed when the subscriber is
// evicted, the hub is closed, or Close is called.
func (s *Subscriber) Events() <-chan Event { return s.ch }

// Close unregisters the subscriber. It is safe to call multiple times and
// after eviction.
func (s *Subscriber) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	delete(s.hub.subs, s)
	close(s.ch)
}
