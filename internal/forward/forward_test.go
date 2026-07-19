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

// oldest returns the i-th oldest buffered entry (test helper).
func (c *Client) oldest(i int) entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ring[(c.head+i)%c.max]
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
	if c.oldest(0).line.Text != "c" || c.oldest(2).line.Text != "e" {
		t.Fatalf("buffer contents wrong: %+v, %+v", c.oldest(0), c.oldest(2))
	}
	if c.oldest(0).seq != 3 {
		t.Fatalf("oldest seq = %d, want 3", c.oldest(0).seq)
	}
}

func TestAckTrimsBuffer(t *testing.T) {
	c := New("http://example.invalid", "tok", Options{BufferLines: 10})
	for i := 0; i < 5; i++ {
		c.Enqueue("src", "line", 0, time.Now())
	}
	c.trimAcked(3)

	st := c.Status()
	if st.BufferedLines != 2 {
		t.Fatalf("buffered after ack = %d, want 2", st.BufferedLines)
	}
	if c.oldest(0).seq != 4 {
		t.Fatalf("oldest remaining seq = %d, want 4", c.oldest(0).seq)
	}
}

func TestRingWrapsCorrectly(t *testing.T) {
	// Drive head far past several wraparounds and verify FIFO order and
	// contents survive (the ring's whole point).
	c := New("http://example.invalid", "tok", Options{BufferLines: 4})
	for i := 0; i < 103; i++ {
		c.Enqueue("src", string(rune('A'+i%26)), 0, time.Now())
	}
	if got := c.Status().BufferedLines; got != 4 {
		t.Fatalf("buffered = %d, want 4", got)
	}
	for i := 0; i < 4; i++ {
		wantSeq := uint64(100 + i)
		e := c.oldest(i)
		if e.seq != wantSeq {
			t.Fatalf("oldest(%d).seq = %d, want %d", i, e.seq, wantSeq)
		}
		if want := string(rune('A' + (99+i)%26)); e.line.Text != want {
			t.Fatalf("oldest(%d).Text = %q, want %q", i, e.line.Text, want)
		}
	}
	// Trimming across the wrap boundary must also work.
	c.trimAcked(101)
	if got := c.Status().BufferedLines; got != 2 {
		t.Fatalf("after trim: %d, want 2", got)
	}
	if c.oldest(0).seq != 102 {
		t.Fatalf("after trim oldest = %d, want 102", c.oldest(0).seq)
	}
}
