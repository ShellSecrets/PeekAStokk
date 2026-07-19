package forward_test

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shellsecrets/peekastokk/internal/forward"
	"github.com/shellsecrets/peekastokk/internal/hub"
	"github.com/shellsecrets/peekastokk/internal/server"
)

// These tests run the real forward.Client against the real server /ingest
// handler over an actual TCP socket (httptest.NewServer binds a real
// port), so the full-duplex chunked-POST pattern is exercised end to end.

func TestForwardDeliversToRealServer(t *testing.T) {
	h := hub.New(100)
	ts := httptest.NewServer(server.New(h, server.Options{
		Files:        []string{"seed.log"},
		Lines:        500,
		IngestTokens: map[string]string{"client-a": "tok-123"},
	}).Handler())
	t.Cleanup(ts.Close)

	sub, _ := h.Subscribe(64, 0, nil)
	defer sub.Close()

	c := forward.New(ts.URL, "tok-123", forward.Options{BufferLines: 100})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// Lines enqueued before the connection is up must survive and arrive
	// (buffered, then flushed on connect).
	c.Enqueue("nginx", "early line", 0, time.Now())
	time.Sleep(300 * time.Millisecond)
	c.Enqueue("nginx", "later line", 10, time.Now())

	var got []hub.Event
	timeout := time.After(10 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-sub.Events():
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("timed out; got %+v, status %+v", got, c.Status())
		}
	}
	if got[0].Text != "early line" || got[1].Text != "later line" {
		t.Fatalf("events = %+v", got)
	}
	if got[0].File != "forward:client-a/nginx" {
		t.Fatalf("hub key = %q", got[0].File)
	}

	// Acks (every ~1s) must trim the client's buffer back to empty.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := c.Status(); st.Connected && st.BufferedLines == 0 && st.LinesSent >= 2 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("buffer never trimmed by acks: %+v", c.Status())
}

func TestForwardWrongTokenReportsAuthError(t *testing.T) {
	h := hub.New(10)
	ts := httptest.NewServer(server.New(h, server.Options{
		Files:        []string{"seed.log"},
		Lines:        500,
		IngestTokens: map[string]string{"client-a": "right-token"},
	}).Handler())
	t.Cleanup(ts.Close)

	c := forward.New(ts.URL, "wrong-token", forward.Options{BufferLines: 10})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := c.Status(); strings.Contains(st.LastError, "rejected") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("auth rejection never surfaced: %+v", c.Status())
}

func TestForwardReconnectsAfterServerRestart(t *testing.T) {
	h1 := hub.New(100)
	opts := server.Options{
		Files:        []string{"seed.log"},
		Lines:        500,
		IngestTokens: map[string]string{"client-a": "tok-123"},
	}
	ts := httptest.NewServer(server.New(h1, opts).Handler())

	c := forward.New(ts.URL, "tok-123", forward.Options{BufferLines: 100})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	c.Enqueue("app", "before restart", 0, time.Now())
	waitFor(t, 10*time.Second, func() bool {
		st := c.Status()
		return st.Connected && st.BufferedLines == 0
	}, "first delivery")

	// Kill the server; the line enqueued while down must be buffered and
	// delivered after a new server appears on the same address.
	// CloseClientConnections first: Close() alone waits for in-flight
	// requests, and the ingest stream is deliberately never-ending.
	addr := ts.Listener.Addr().String()
	ts.CloseClientConnections()
	ts.Close()
	c.Enqueue("app", "while down", 0, time.Now())
	time.Sleep(500 * time.Millisecond)

	h2 := hub.New(100)
	sub, _ := h2.Subscribe(64, 0, nil)
	defer sub.Close()
	ts2 := httptest.NewUnstartedServer(server.New(h2, opts).Handler())
	ts2.Listener.Close()
	ts2.Listener = newListenerOn(t, addr)
	ts2.Start()
	t.Cleanup(func() {
		ts2.CloseClientConnections()
		ts2.Close()
	})

	timeout := time.After(60 * time.Second) // reconnect backoff can take a few cycles
	for {
		select {
		case ev := <-sub.Events():
			if ev.Text == "while down" {
				return // delivered after reconnect
			}
		case <-timeout:
			t.Fatalf("line never delivered after restart: %+v", c.Status())
		}
	}
}

// newListenerOn rebinds the exact address a closed test server used, so a
// "restarted server" appears where the client is already reconnecting to.
func newListenerOn(t *testing.T, addr string) net.Listener {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := net.Listen("tcp", addr)
		if err == nil {
			return l
		}
		if time.Now().After(deadline) {
			t.Fatalf("rebinding %s: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
