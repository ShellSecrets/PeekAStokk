package forward_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestRemoteScrollbackEndToEnd: a browser-style /api/before request for a
// forwarded source is relayed over the live ingest connection to the real
// client, which reads its own disk and answers — the full remote
// scrollback path over a real socket.
func TestRemoteScrollbackEndToEnd(t *testing.T) {
	// A real file on the "client's" disk with docker-json content, so the
	// unwrap-consistency guarantee is exercised remotely too.
	dir := t.TempDir()
	path := filepath.Join(dir, "remote.log")
	var content string
	for i := range 50 {
		content += fmt.Sprintf(`{"log":"remote event %d\n","stream":"stdout","time":"2026-07-19T06:00:00Z"}`+"\n", i)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	h := hub.New(100)
	ts := httptest.NewServer(server.New(h, server.Options{
		Files:        []string{"seed.log"},
		Lines:        500,
		IngestTokens: map[string]string{"client-a": "tok-123"},
	}).Handler())
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})

	c := forward.New(ts.URL, "tok-123", forward.Options{
		BufferLines: 100,
		ResolvePath: func(source string) (string, bool) {
			if source == "remote-app" {
				return path, true
			}
			return "", false
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// One live line registers the source on the server.
	c.Enqueue("remote-app", "live line", int64(len(content)), time.Now())

	// Find the source's opaque id via /api/files.
	var id string
	waitFor(t, 10*time.Second, func() bool {
		resp, err := http.Get(ts.URL + "/api/files")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var body struct {
			Files []struct{ ID, Name string } `json:"files"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		for _, f := range body.Files {
			if f.Name == "client-a/remote-app" {
				id = f.ID
				return true
			}
		}
		return false
	}, "forwarded source registration")

	// Scroll back: ask for the 10 lines before offset EOF-ish anchor.
	resp, err := http.Get(ts.URL + "/api/before?file=" + id + "&limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Lines []struct {
			Off  int64  `json:"off"`
			Text string `json:"text"`
		} `json:"lines"`
		AtStart bool `json:"atStart"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Lines) != 10 {
		t.Fatalf("got %d lines, want 10: %+v", len(body.Lines), body)
	}
	// Newest 10 of 50, unwrapped from docker-json, oldest first.
	for i, ln := range body.Lines {
		want := fmt.Sprintf("remote event %d", 40+i)
		if ln.Text != want {
			t.Fatalf("lines[%d] = %q, want %q", i, ln.Text, want)
		}
	}
	if body.AtStart {
		t.Fatal("atStart should be false with 40 older lines remaining")
	}

	// Page all the way back to the start using the offset anchors.
	anchor := body.Lines[0].Off
	total := 10
	for !body.AtStart {
		resp, err := http.Get(fmt.Sprintf("%s/api/before?file=%s&limit=20&offset=%d", ts.URL, id, anchor))
		if err != nil {
			t.Fatal(err)
		}
		body.Lines = nil
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if len(body.Lines) == 0 {
			break
		}
		total += len(body.Lines)
		anchor = body.Lines[0].Off
	}
	if total != 50 || anchor != 0 {
		t.Fatalf("paged %d lines to offset %d, want 50 to 0", total, anchor)
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

// A server that restarts loses its registry; the client's connection is
// re-established, but a file that is not being written to would have no
// reason to send anything. The announced source list is what puts those
// files back in the picker.
func TestForwardAnnouncesSourcesWithoutLines(t *testing.T) {
	ts := httptest.NewServer(server.New(hub.New(100), server.Options{
		Files:        []string{"seed.log"},
		Lines:        500,
		IngestTokens: map[string]string{"client-a": "tok-123"},
	}).Handler())
	t.Cleanup(ts.Close)

	c := forward.New(ts.URL, "tok-123", forward.Options{
		BufferLines: 100,
		Sources:     func() []string { return []string{"quiet.log", "busy.log"} },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	want := map[string]bool{"client-a/quiet.log": true, "client-a/busy.log": true}
	deadline := time.Now().Add(10 * time.Second)
	for {
		names := fileNames(t, ts.URL)
		missing := false
		for w := range want {
			if !names[w] {
				missing = true
			}
		}
		if !missing {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("announced sources never registered; file list = %v, status %+v", names, c.Status())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func fileNames(t *testing.T, baseURL string) map[string]bool {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/files")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(body.Files))
	for _, f := range body.Files {
		names[f.Name] = true
	}
	return names
}
