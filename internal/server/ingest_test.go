package server_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shellsecrets/peekastokk/internal/hub"
	"github.com/shellsecrets/peekastokk/internal/ingestproto"
	"github.com/shellsecrets/peekastokk/internal/server"
)

func newIngestServer(t *testing.T, h *hub.Hub) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(server.New(h, server.Options{
		Files:        []string{"local.log"},
		Lines:        500,
		IngestTokens: map[string]string{"homelab-1": "sekrit-token"},
	}).Handler())
	t.Cleanup(ts.Close)
	return ts
}

// startIngest opens an /ingest connection and returns a writer for NDJSON
// lines plus the response for reading acks.
func startIngest(t *testing.T, ts *httptest.Server, token string) (io.WriteCloser, *http.Response) {
	t.Helper()
	pr, pw := io.Pipe()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/ingest", pr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pw.Close(); resp.Body.Close() })
	return pw, resp
}

func sendLine(t *testing.T, w io.Writer, ln ingestproto.Line) {
	t.Helper()
	data, err := json.Marshal(ln)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
}

func TestIngestRejectsBadAuth(t *testing.T) {
	ts := newIngestServer(t, hub.New(10))
	for name, token := range map[string]string{"missing": "", "wrong": "nope"} {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/ingest", strings.NewReader(""))
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

func TestIngestRouteAbsentWithoutTokens(t *testing.T) {
	ts := httptest.NewServer(server.New(hub.New(10), server.Options{Files: []string{"a.log"}, Lines: 500}).Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Post(ts.URL+"/ingest", "application/x-ndjson", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no ingest tokens configured", resp.StatusCode)
	}
}

func TestIngestDeliversToSubscribersAndRegisters(t *testing.T) {
	h := hub.New(10)
	ts := newIngestServer(t, h)

	sub, _ := h.Subscribe(16, 0, nil)
	defer sub.Close()

	w, resp := startIngest(t, ts, "sekrit-token")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	sendLine(t, w, ingestproto.Line{Seq: 1, Source: "nginx", Text: "hello from afar", Time: time.Now()})
	// A malformed line in between must not kill the stream.
	if _, err := io.WriteString(w, "{torn json\n"); err != nil {
		t.Fatal(err)
	}
	sendLine(t, w, ingestproto.Line{Seq: 2, Source: "nginx", Text: "still alive", Time: time.Now()})

	var got []hub.Event
	timeout := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-sub.Events():
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("timed out, got %+v", got)
		}
	}
	if got[0].Text != "hello from afar" || got[1].Text != "still alive" {
		t.Fatalf("events = %+v", got)
	}
	if got[0].File != "forward:homelab-1/nginx" {
		t.Fatalf("hub key = %q", got[0].File)
	}

	// The source must now be listed with the client-prefixed display name.
	fresp, err := http.Get(ts.URL + "/api/files")
	if err != nil {
		t.Fatal(err)
	}
	defer fresp.Body.Close()
	var body struct {
		Files []struct{ ID, Name string } `json:"files"`
	}
	if err := json.NewDecoder(fresp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range body.Files {
		if f.Name == "homelab-1/nginx" {
			found = true
		}
	}
	if !found {
		t.Fatalf("forwarded source not in /api/files: %+v", body.Files)
	}
}

func TestIngestSendsAcks(t *testing.T) {
	h := hub.New(10)
	ts := newIngestServer(t, h)
	w, resp := startIngest(t, ts, "sekrit-token")

	sendLine(t, w, ingestproto.Line{Seq: 7, Source: "app", Text: "x", Time: time.Now()})

	// Read acks until one covers seq 7 (ack ticker fires every second).
	reader := bufio.NewReader(resp.Body)
	deadline := time.After(10 * time.Second)
	ackCh := make(chan ingestproto.Ack, 16)
	go func() {
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				close(ackCh)
				return
			}
			var ack ingestproto.Ack
			if json.Unmarshal(line, &ack) == nil {
				ackCh <- ack
			}
		}
	}()
	for {
		select {
		case ack, open := <-ackCh:
			if !open {
				t.Fatal("ack stream closed early")
			}
			if ack.Ack >= 7 {
				return // success
			}
		case <-deadline:
			t.Fatal("never received an ack covering seq 7")
		}
	}
}

func TestBackfillShortCircuitsForwardedEntries(t *testing.T) {
	h := hub.New(10)
	ts := newIngestServer(t, h)
	w, _ := startIngest(t, ts, "sekrit-token")
	sendLine(t, w, ingestproto.Line{Seq: 1, Source: "remote-app", Text: "line", Time: time.Now()})

	// Wait until the source is registered, find its id.
	var id string
	deadline := time.Now().Add(5 * time.Second)
	for id == "" && time.Now().Before(deadline) {
		resp, err := http.Get(ts.URL + "/api/files")
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Files []struct{ ID, Name string } `json:"files"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		for _, f := range body.Files {
			if f.Name == "homelab-1/remote-app" {
				id = f.ID
			}
		}
		if id == "" {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if id == "" {
		t.Fatal("forwarded source never registered")
	}

	status, body := getBefore(t, ts, url.Values{"file": {id}})
	if status != http.StatusOK || len(body.Lines) != 0 || !body.AtStart {
		t.Fatalf("forwarded backfill = %d %+v, want empty atStart", status, body)
	}
}

func TestRegisterSourceConcurrent(t *testing.T) {
	s := server.New(hub.New(10), server.Options{Files: []string{"seed.log"}, Lines: 500})

	const workers = 16
	var wg sync.WaitGroup
	ids := make([][]string, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				// Half shared keys (contended), half unique per worker.
				key := fmt.Sprintf("shared-%d", i%10)
				if i%2 == 1 {
					key = fmt.Sprintf("unique-%d-%d", w, i)
				}
				id, _ := s.RegisterSource(key, key, true)
				ids[w] = append(ids[w], key+"="+id)
			}
		}(w)
	}
	wg.Wait()

	// The same key must have resolved to the same id in every goroutine.
	seen := make(map[string]string)
	for _, list := range ids {
		for _, pair := range list {
			k, id, _ := strings.Cut(pair, "=")
			if prev, ok := seen[k]; ok && prev != id {
				t.Fatalf("key %s got two ids: %s and %s", k, prev, id)
			}
			seen[k] = id
		}
	}
}
