package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shellsecrets/peekastokk/internal/hub"
	"github.com/shellsecrets/peekastokk/internal/server"
)

func newTestServer(t *testing.T, h *hub.Hub) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(server.New(h, []string{"a.log", "b.log"}, 500, nil).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t, hub.New(10))
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestIndexServesUI(t *testing.T) {
	ts := newTestServer(t, hub.New(10))
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	ts := newTestServer(t, hub.New(10))
	resp, err := http.Get(ts.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestFilesEndpoint(t *testing.T) {
	ts := newTestServer(t, hub.New(10))
	resp, err := http.Get(ts.URL + "/api/files")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body struct {
		Files []string `json:"files"`
		Lines int      `json:"lines"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Files) != 2 || body.Files[0] != "a.log" {
		t.Fatalf("files = %v", body.Files)
	}
	if body.Lines != 500 {
		t.Fatalf("lines = %d, want 500", body.Lines)
	}
}

// readEvents reads SSE from r until n data events have been parsed.
func readEvents(t *testing.T, r *bufio.Reader, n int) []hub.Event {
	t.Helper()
	var events []hub.Event
	for len(events) < n {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended early: %v (got %d/%d events)", err, len(events), n)
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var ev hub.Event
			if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &ev); err != nil {
				t.Fatalf("bad event payload %q: %v", data, err)
			}
			events = append(events, ev)
		}
	}
	return events
}

func TestEventStreamReplaysHistoryAndStreamsLive(t *testing.T) {
	h := hub.New(10)
	h.Publish("a.log", "first", 0, time.Now())
	h.Publish("a.log", "second", 0, time.Now())
	ts := newTestServer(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	history := readEvents(t, reader, 2)
	if history[0].Text != "first" || history[1].Text != "second" {
		t.Fatalf("history = %+v", history)
	}

	h.Publish("b.log", "live", 0, time.Now())
	live := readEvents(t, reader, 1)
	if live[0].Text != "live" || live[0].Seq != 3 {
		t.Fatalf("live = %+v", live)
	}
}

func TestEventStreamHonorsLastEventID(t *testing.T) {
	h := hub.New(10)
	h.Publish("a.log", "first", 0, time.Now())
	h.Publish("a.log", "second", 0, time.Now())
	ts := newTestServer(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	req.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	events := readEvents(t, bufio.NewReader(resp.Body), 1)
	if events[0].Seq != 2 || events[0].Text != "second" {
		t.Fatalf("events = %+v", events)
	}
}

func TestEventStreamFilesFilter(t *testing.T) {
	h := hub.New(10)
	h.Publish("a.log", "from-a", 0, time.Now())
	h.Publish("b.log", "from-b", 0, time.Now())
	ts := newTestServer(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events?files=b.log", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	history := readEvents(t, reader, 1)
	if history[0].Text != "from-b" {
		t.Fatalf("history = %+v", history)
	}

	h.Publish("a.log", "live-a", 0, time.Now())
	h.Publish("b.log", "live-b", 0, time.Now())
	live := readEvents(t, reader, 1)
	if live[0].Text != "live-b" {
		t.Fatalf("live = %+v, want only b.log events", live[0])
	}
}

func TestEventStreamAfterParam(t *testing.T) {
	h := hub.New(10)
	h.Publish("a.log", "first", 0, time.Now())
	h.Publish("a.log", "second", 0, time.Now())
	ts := newTestServer(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events?after=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	events := readEvents(t, bufio.NewReader(resp.Body), 1)
	if events[0].Seq != 2 || events[0].Text != "second" {
		t.Fatalf("events = %+v", events)
	}
}

func TestEventStreamRejectsBadFilesFilter(t *testing.T) {
	ts := newTestServer(t, hub.New(10))
	for _, q := range []string{"?files=nope.log", "?files="} {
		resp, err := http.Get(ts.URL + "/events" + q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestEventStreamEndsWhenHubCloses(t *testing.T) {
	h := hub.New(10)
	ts := newTestServer(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	h.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 512)
		for {
			if _, err := resp.Body.Read(buf); err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not end after hub close")
	}
}
