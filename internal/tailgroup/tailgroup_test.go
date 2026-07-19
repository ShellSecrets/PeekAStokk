package tailgroup_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shellsecrets/peekastokk/internal/tail"
	"github.com/shellsecrets/peekastokk/internal/tailgroup"
)

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileStartsAndStopsTailers(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	b := filepath.Join(dir, "b.log")
	appendTo(t, a, "")
	appendTo(t, b, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan tail.Line, 64)
	g := tailgroup.NewGroup(ctx, out, tail.Options{PollInterval: 10 * time.Millisecond})
	defer g.Stop()

	g.Reconcile([]string{a})
	appendTo(t, a, "from a\n")
	select {
	case ln := <-out:
		if ln.Text != "from a" || ln.File != a {
			t.Fatalf("got %+v", ln)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tailer for a never delivered")
	}

	// Add b, drop a.
	g.Reconcile([]string{b})
	appendTo(t, b, "from b\n")
	select {
	case ln := <-out:
		if ln.Text != "from b" || ln.File != b {
			t.Fatalf("got %+v", ln)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tailer for b never delivered")
	}

	// Give a's cancelled tailer time to observe cancellation, then write
	// to a: nothing may arrive.
	time.Sleep(100 * time.Millisecond)
	appendTo(t, a, "should not arrive\n")
	select {
	case ln := <-out:
		t.Fatalf("removed tailer still delivering: %+v", ln)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	appendTo(t, a, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan tail.Line, 64)
	g := tailgroup.NewGroup(ctx, out, tail.Options{PollInterval: 10 * time.Millisecond})
	defer g.Stop()

	// Repeated reconciles with the same set must not spawn duplicate
	// tailers (which would show as duplicate lines).
	for i := 0; i < 5; i++ {
		g.Reconcile([]string{a})
	}
	appendTo(t, a, "once\n")

	seen := 0
	deadline := time.After(1 * time.Second)
	for {
		select {
		case <-out:
			seen++
		case <-deadline:
			if seen != 1 {
				t.Fatalf("line delivered %d times, want exactly 1", seen)
			}
			return
		}
	}
}

func TestStopJoinsCleanly(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	appendTo(t, a, "")

	out := make(chan tail.Line, 64)
	g := tailgroup.NewGroup(context.Background(), out, tail.Options{PollInterval: 10 * time.Millisecond})
	g.Reconcile([]string{a})

	done := make(chan struct{})
	go func() {
		g.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not join")
	}
}
