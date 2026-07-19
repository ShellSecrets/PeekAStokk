package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shellsecrets/peekastokk/internal/forward"
)

func TestStatusHandlerHealthzAlways(t *testing.T) {
	ts := httptest.NewServer(newStatusHandler(nil, nil))
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body = %v", body)
	}
}

func TestStatusHandlerStatuszOnlyWithForwarder(t *testing.T) {
	// Without a forwarder configured, /statusz must not exist.
	bare := httptest.NewServer(newStatusHandler(nil, nil))
	t.Cleanup(bare.Close)
	resp, err := http.Get(bare.URL + "/statusz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("statusz without forwarder = %d, want 404", resp.StatusCode)
	}

	// With one, it reports the client's state as JSON.
	fwd := forward.New("http://example.invalid", "tok", forward.Options{BufferLines: 10})
	fwd.Enqueue("src", "one line", 0, time.Now())
	withFwd := httptest.NewServer(newStatusHandler(nil, fwd))
	t.Cleanup(withFwd.Close)

	resp, err = http.Get(withFwd.URL + "/statusz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("statusz = %d", resp.StatusCode)
	}
	var st forward.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Connected || st.BufferedLines != 1 {
		t.Fatalf("status = %+v, want disconnected with 1 buffered line", st)
	}
}
