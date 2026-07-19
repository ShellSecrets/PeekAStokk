package forward

import (
	"testing"
	"time"
)

func TestBackoffGrowsAndCaps(t *testing.T) {
	for attempt := 0; attempt < 12; attempt++ {
		max := backoffCap
		if attempt < 5 {
			if d := backoffBase << uint(attempt); d < max {
				max = d
			}
		}
		for i := 0; i < 50; i++ {
			d := Backoff(attempt)
			if d < max/2 || d > max {
				t.Fatalf("attempt %d: delay %v outside [%v, %v]", attempt, d, max/2, max)
			}
		}
	}
}

func TestEnqueueRingOverflow(t *testing.T) {
	c := New("http://example.invalid", "tok", Options{BufferLines: 3})
	for i := 0; i < 5; i++ {
		c.Enqueue("src", string(rune('a'+i)), 0, time.Now())
	}
	st := c.Status()
	if st.BufferedLines != 3 {
		t.Fatalf("buffered = %d, want 3", st.BufferedLines)
	}
	if st.DroppedLines != 2 {
		t.Fatalf("dropped = %d, want 2", st.DroppedLines)
	}
	// Oldest dropped: remaining are c, d, e with seqs 3..5.
	if c.buf[0].line.Text != "c" || c.buf[2].line.Text != "e" {
		t.Fatalf("buffer contents wrong: %+v", c.buf)
	}
	if c.buf[0].seq != 3 {
		t.Fatalf("oldest seq = %d, want 3", c.buf[0].seq)
	}
}

func TestAckTrimsBuffer(t *testing.T) {
	c := New("http://example.invalid", "tok", Options{BufferLines: 10})
	for i := 0; i < 5; i++ {
		c.Enqueue("src", "line", 0, time.Now())
	}
	// Simulate what the ack reader does for ack=3.
	c.mu.Lock()
	i := 0
	for i < len(c.buf) && c.buf[i].seq <= 3 {
		i++
	}
	c.buf = c.buf[i:]
	c.mu.Unlock()

	st := c.Status()
	if st.BufferedLines != 2 {
		t.Fatalf("buffered after ack = %d, want 2", st.BufferedLines)
	}
	if c.buf[0].seq != 4 {
		t.Fatalf("oldest remaining seq = %d, want 4", c.buf[0].seq)
	}
}
