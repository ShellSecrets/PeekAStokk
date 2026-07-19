package forward

import (
	"testing"
	"time"
)

// BenchmarkEnqueueSteadyOverflow measures Enqueue with the buffer
// permanently full — the disconnected-under-load case, where every call
// drops the oldest entry.
func BenchmarkEnqueueSteadyOverflow(b *testing.B) {
	c := New("http://example.invalid", "tok", Options{BufferLines: 5000})
	ts := time.Now()
	for i := 0; i < 5000; i++ {
		c.Enqueue("src", "prefill line for the ring", 0, ts)
	}
	b.ReportAllocs()
	for b.Loop() {
		c.Enqueue("src", "steady-state overflow line", 0, ts)
	}
}
