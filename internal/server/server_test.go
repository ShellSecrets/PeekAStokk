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

	"github.com/shellsecrets/peekastokk/internal/auth"
	"github.com/shellsecrets/peekastokk/internal/hub"
	"github.com/shellsecrets/peekastokk/internal/server"
)

func newTestServer(t *testing.T, h *hub.Hub) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(server.New(h, server.Options{Files: []string{"a.log", "b.log"}, Lines: 500}).Handler())
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

func TestBasicAuth(t *testing.T) {
	h := hub.New(10)
	h.Publish("a.log", "secret line", 0, time.Now())
	ts := httptest.NewServer(server.New(h, server.Options{
		Files: []string{"a.log"}, Lines: 500,
		AuthUser: "dev", AuthPass: "s3cret",
	}).Handler())
	t.Cleanup(ts.Close)

	get := func(path, user, pass string) *http.Response {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Every surface is protected...
	for _, path := range []string{"/", "/api/files", "/api/before?file=0", "/events"} {
		resp := get(path, "", "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without creds: status = %d, want 401", path, resp.StatusCode)
		}
		if !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Basic") {
			t.Errorf("%s: missing WWW-Authenticate challenge", path)
		}
	}

	// ...wrong credentials are rejected...
	for _, c := range [][2]string{{"dev", "wrong"}, {"wrong", "s3cret"}, {"", "s3cret"}} {
		resp := get("/api/files", c[0]+"x", c[1]) // +x guards the empty-user case
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("creds %v: status = %d, want 401", c, resp.StatusCode)
		}
	}

	// ...correct credentials work, including on the stream.
	resp := get("/api/files", "dev", "s3cret")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("valid creds: status = %d, want 200", resp.StatusCode)
	}
	stream := get("/events", "dev", "s3cret")
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Errorf("stream with creds: status = %d", stream.StatusCode)
	}
	events := readEvents(t, bufio.NewReader(stream.Body), 1)
	if events[0].Text != "secret line" {
		t.Errorf("stream = %+v", events[0])
	}

	// Health checks stay open for load balancers.
	resp = get("/healthz", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz should not require auth, got %d", resp.StatusCode)
	}
}

func TestBasicAuthWithArgon2Hash(t *testing.T) {
	hash, err := auth.HashPassword("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(hub.New(10), server.Options{
		Files: []string{"a.log"}, Lines: 500,
		AuthUser: "dev", AuthPass: hash,
	}).Handler())
	t.Cleanup(ts.Close)

	get := func(user, pass string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/files", nil)
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := get("", ""); code != http.StatusUnauthorized {
		t.Errorf("no creds: %d, want 401", code)
	}
	if code := get("dev", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong password: %d, want 401", code)
	}
	// First correct attempt runs the slow KDF; the second hits the cache.
	for i := 0; i < 2; i++ {
		if code := get("dev", "s3cret"); code != http.StatusOK {
			t.Errorf("attempt %d with correct password: %d, want 200", i+1, code)
		}
	}
	// The cache must not have weakened anything.
	if code := get("dev", "still wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong password after cache warm: %d, want 401", code)
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
	ts := httptest.NewServer(server.New(h, server.Options{Files: []string{path}, Lines: 500}).Handler())
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

// TestUnregisterHidesEntryAndReRegisterRevivesIt covers the dynamic
// directory-watching contract: a deleted file disappears from /api/files
// but keeps its id, and the same path re-registering gets it back.
func TestUnregisterHidesEntryAndReRegisterRevivesIt(t *testing.T) {
	srv := server.New(hub.New(10), server.Options{Files: []string{"a.log"}, Lines: 500})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	list := func() []string {
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
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(body.Files))
		for i, f := range body.Files {
			out[i] = f.ID + ":" + f.Name
		}
		return out
	}

	id, isNew := srv.RegisterSource("/tmp/w/new.log", "new.log", true)
	if !isNew {
		t.Fatalf("RegisterSource reported existing for a new path")
	}
	if got := list(); len(got) != 2 || got[1] != id+":new.log" {
		t.Fatalf("after register: %v", got)
	}

	srv.UnregisterSource("/tmp/w/new.log")
	if got := list(); len(got) != 1 || got[0] != "0:a.log" {
		t.Fatalf("after unregister: %v", got)
	}

	revived, isNew := srv.RegisterSource("/tmp/w/new.log", "new.log", true)
	if isNew || revived != id {
		t.Fatalf("revived id = %q (isNew=%v), want %q", revived, isNew, id)
	}
	if got := list(); len(got) != 2 || got[1] != id+":new.log" {
		t.Fatalf("after revival: %v", got)
	}

	// Unregistering an unknown key must be a harmless no-op.
	srv.UnregisterSource("/never/registered")
	if got := list(); len(got) != 2 {
		t.Fatalf("after no-op unregister: %v", got)
	}
}

func TestFilesUseNameOverrides(t *testing.T) {
	ts := httptest.NewServer(server.New(hub.New(10), server.Options{
		Files: []string{"/var/log/w1/app.log", "/var/log/w2/app.log", "/var/log/plain.log"},
		Names: map[string]string{
			"/var/log/w1/app.log": "worker1/app.log",
			"/var/log/w2/app.log": "worker2/app.log",
		},
		Lines: 500,
	}).Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/files")
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
	want := []string{"worker1/app.log", "worker2/app.log", "plain.log"}
	if len(body.Files) != len(want) {
		t.Fatalf("files = %v, want %v", body.Files, want)
	}
	for i, w := range want {
		if body.Files[i].Name != w {
			t.Fatalf("files = %v, want %v", body.Files, want)
		}
	}
}
