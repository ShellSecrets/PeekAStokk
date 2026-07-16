package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		Files []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"files"`
		Lines int `json:"lines"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Files) != 2 || body.Files[0].ID != "0" || body.Files[0].Name != "a.log" {
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
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events?files=1", nil)
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

// TestReplayFlagMarksHistoryOnly: history sent on connect is flagged
// replay:true (the UI hides its meaningless timestamps); live events are
// not flagged.
func TestReplayFlagMarksHistoryOnly(t *testing.T) {
	h := hub.New(10)
	h.Publish("a.log", "old", 0, time.Now())
	ts := newTestServer(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	readData := func(r *bufio.Reader) string {
		t.Helper()
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended: %v", err)
			}
			if strings.HasPrefix(line, "data: ") {
				return line
			}
		}
	}

	reader := bufio.NewReader(resp.Body)
	if history := readData(reader); !strings.Contains(history, `"replay":true`) {
		t.Errorf("history event not marked replay: %s", history)
	}
	h.Publish("a.log", "fresh", 0, time.Now())
	if live := readData(reader); strings.Contains(live, `"replay"`) {
		t.Errorf("live event wrongly marked replay: %s", live)
	}
}

// TestNoPathDisclosure locks in the privacy contract: absolute paths of
// tailed files must never appear in anything sent to a client.
func TestNoPathDisclosure(t *testing.T) {
	dir := t.TempDir() // a distinctive absolute path
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("on disk\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := hub.New(10)
	h.Publish(path, "streamed line", 0, time.Now())
	ts := httptest.NewServer(server.New(h, []string{path}, 500, nil).Handler())
	t.Cleanup(ts.Close)

	fetch := func(url string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 64*1024)
		n, _ := resp.Body.Read(buf)
		return string(buf[:n])
	}

	for name, url := range map[string]string{
		"api/files":  ts.URL + "/api/files",
		"events":     ts.URL + "/events",
		"api/before": ts.URL + "/api/before?file=0",
	} {
		body := fetch(url)
		if strings.Contains(body, dir) {
			t.Errorf("%s response leaks the file path:\n%s", name, body)
		}
		if body == "" {
			t.Errorf("%s returned an empty body", name)
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
