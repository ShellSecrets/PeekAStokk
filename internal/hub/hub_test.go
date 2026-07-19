package hub_test

import (
	"testing"
	"time"

	"github.com/shellsecrets/peekastokk/internal/hub"
)

func publishN(h *hub.Hub, n int) {
	for i := 0; i < n; i++ {
		h.Publish("app.log", "line", 0, time.Now())
	}
}

func TestHistoryReplay(t *testing.T) {
	h := hub.New(10)
	publishN(h, 3)

	_, history := h.Subscribe(8, 0, nil)
	if len(history) != 3 {
		t.Fatalf("history length = %d, want 3", len(history))
	}
	for i, ev := range history {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("history[%d].Seq = %d, want %d", i, ev.Seq, i+1)
		}
	}
}

func TestHistoryAfterSeqSkipsSeenEvents(t *testing.T) {
	h := hub.New(10)
	publishN(h, 5)

	_, history := h.Subscribe(8, 3, nil)
	if len(history) != 2 || history[0].Seq != 4 || history[1].Seq != 5 {
		t.Fatalf("unexpected history %+v", history)
	}
}

func TestHistoryRingDropsOldest(t *testing.T) {
	h := hub.New(2)
	publishN(h, 5)

	_, history := h.Subscribe(8, 0, nil)
	if len(history) != 2 || history[0].Seq != 4 || history[1].Seq != 5 {
		t.Fatalf("unexpected history %+v", history)
	}
}

func TestBroadcastReachesAllSubscribers(t *testing.T) {
	h := hub.New(0)
	a, _ := h.Subscribe(8, 0, nil)
	b, _ := h.Subscribe(8, 0, nil)

	h.Publish("app.log", "hello", 0, time.Now())

	for _, sub := range []*hub.Subscriber{a, b} {
		select {
		case ev := <-sub.Events():
			if ev.Text != "hello" {
				t.Fatalf("got %q", ev.Text)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out")
		}
	}
}

func TestFileFilterRestrictsHistoryAndDelivery(t *testing.T) {
	h := hub.New(10)
	h.Publish("a.log", "old-a", 0, time.Now())
	h.Publish("b.log", "old-b", 0, time.Now())

	sub, history := h.Subscribe(8, 0, []string{"b.log"})
	if len(history) != 1 || history[0].Text != "old-b" {
		t.Fatalf("history = %+v", history)
	}

	h.Publish("a.log", "live-a", 0, time.Now())
	h.Publish("b.log", "live-b", 0, time.Now())

	select {
	case ev := <-sub.Events():
		if ev.Text != "live-b" {
			t.Fatalf("got %q, want live-b only", ev.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("unexpected extra event %+v", ev)
	default:
	}
}

func TestSlowSubscriberIsEvicted(t *testing.T) {
	h := hub.New(0)
	slow, _ := h.Subscribe(1, 0, nil)
	publishN(h, 3) // buffer holds 1; the second publish evicts

	if ev, open := <-slow.Events(); !open || ev.Seq != 1 {
		t.Fatalf("first receive = (%+v, %v)", ev, open)
	}
	if _, open := <-slow.Events(); open {
		t.Fatal("expected channel to be closed after eviction")
	}
	slow.Close() // must be safe after eviction
}

func TestCloseShutsDownSubscribers(t *testing.T) {
	h := hub.New(0)
	sub, _ := h.Subscribe(8, 0, nil)

	h.Close()
	if _, open := <-sub.Events(); open {
		t.Fatal("expected closed channel")
	}

	if h.Publish("app.log", "after close", 0, time.Now()) { // must not panic
		t.Error("Publish on a closed hub must report false")
	}
	sub.Close() // must not double-close
	h.Close()   // idempotent

	late, history := h.Subscribe(8, 0, nil)
	if len(history) != 0 {
		t.Fatalf("unexpected history %+v", history)
	}
	if _, open := <-late.Events(); open {
		t.Fatal("late subscriber should get a closed channel")
	}
}
