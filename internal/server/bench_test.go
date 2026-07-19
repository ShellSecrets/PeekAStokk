package server

import (
	"io"
	"testing"
	"time"

	"github.com/shellsecrets/peekastokk/internal/hub"
)

// BenchmarkWriteEvent measures the per-event serialization cost on the SSE
// hot path: every log line is written once per connected viewer.
func BenchmarkWriteEvent(b *testing.B) {
	s := New(hub.New(10), Options{Files: []string{"/var/log/app.log"}, Lines: 500})
	ev := hub.Event{
		Seq:  123456,
		File: "/var/log/app.log",
		Text: "GET /api/users 200 12ms request served",
		Off:  987654,
		Time: time.Now(),
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := s.writeEvent(io.Discard, ev, false); err != nil {
			b.Fatal(err)
		}
	}
}
