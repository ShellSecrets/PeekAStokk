package hub

import (
	"testing"
	"time"
)

// BenchmarkPublishFanout measures Publish with subscribers attached and
// draining, the per-line cost on the ingestion path.
func BenchmarkPublishFanout(b *testing.B) {
	h := New(2000)
	for range 4 {
		sub, _ := h.Subscribe(4096, 0, nil)
		go func() {
			for range sub.Events() {
			}
		}()
		defer sub.Close()
	}
	ts := time.Now()
	b.ReportAllocs()
	for b.Loop() {
		h.Publish("/var/log/app.log", "GET /api/users 200 12ms request served", 42, ts)
	}
}
