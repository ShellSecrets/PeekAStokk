package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/shellsecrets/peekastokk/internal/hub"
	"github.com/shellsecrets/peekastokk/internal/server"
)

type beforeResponse struct {
	File  string `json:"file"`
	Lines []struct {
		Off  int64  `json:"off"`
		Text string `json:"text"`
	} `json:"lines"`
	AtStart bool `json:"atStart"`
}

// newBackfillServer serves a real temp log file containing
// "line-0\n" … "line-9\n" (7 bytes each) and returns its path.
func newBackfillServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.log")
	var content string
	for i := 0; i < 10; i++ {
		content += fmt.Sprintf("line-%d\n", i)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(hub.New(10), []string{path}, 500, nil).Handler())
	t.Cleanup(ts.Close)
	return ts, path
}

func getBefore(t *testing.T, ts *httptest.Server, params url.Values) (int, beforeResponse) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/before?" + params.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body beforeResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, body
}

func TestBackfillPagesBackwardsFromAnchor(t *testing.T) {
	ts, path := newBackfillServer(t)

	// Anchor at line-5 (offset 35): the 3 lines before it are 2, 3, 4.
	status, body := getBefore(t, ts, url.Values{
		"file": {path}, "offset": {"35"}, "limit": {"3"},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(body.Lines) != 3 || body.AtStart {
		t.Fatalf("body = %+v", body)
	}
	for i, want := range []struct {
		off  int64
		text string
	}{{14, "line-2"}, {21, "line-3"}, {28, "line-4"}} {
		if body.Lines[i].Off != want.off || body.Lines[i].Text != want.text {
			t.Errorf("lines[%d] = %+v, want %+v", i, body.Lines[i], want)
		}
	}

	// Page again from the new oldest offset: reaches the file start.
	status, body = getBefore(t, ts, url.Values{
		"file": {path}, "offset": {"14"}, "limit": {"5"},
	})
	if status != http.StatusOK || len(body.Lines) != 2 || !body.AtStart {
		t.Fatalf("second page = %d %+v", status, body)
	}
	if body.Lines[0].Off != 0 || body.Lines[0].Text != "line-0" {
		t.Fatalf("lines[0] = %+v", body.Lines[0])
	}
}

func TestBackfillWithoutOffsetAnchorsAtEOF(t *testing.T) {
	ts, path := newBackfillServer(t)
	status, body := getBefore(t, ts, url.Values{"file": {path}})
	if status != http.StatusOK || len(body.Lines) != 10 || !body.AtStart {
		t.Fatalf("got %d, %d lines, atStart=%v", status, len(body.Lines), body.AtStart)
	}
	if body.Lines[9].Text != "line-9" {
		t.Fatalf("last = %+v", body.Lines[9])
	}
}

func TestBackfillAtOffsetZeroIsEmptyAndAtStart(t *testing.T) {
	ts, path := newBackfillServer(t)
	status, body := getBefore(t, ts, url.Values{"file": {path}, "offset": {"0"}})
	if status != http.StatusOK || len(body.Lines) != 0 || !body.AtStart {
		t.Fatalf("got %d %+v", status, body)
	}
}

func TestBackfillDropsUnterminatedTrailingLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.log")
	if err := os.WriteFile(path, []byte("done\nno newline yet"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(hub.New(10), []string{path}, 500, nil).Handler())
	t.Cleanup(ts.Close)

	status, body := getBefore(t, ts, url.Values{"file": {path}})
	if status != http.StatusOK || len(body.Lines) != 1 || body.Lines[0].Text != "done" {
		t.Fatalf("got %d %+v", status, body)
	}
}

func TestBackfillRefusesUntailedFiles(t *testing.T) {
	ts, _ := newBackfillServer(t)

	// A real file that exists but is not tailed must not be readable.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{secret, "/etc/passwd", "", "../app.log"} {
		if status, _ := getBefore(t, ts, url.Values{"file": {file}}); status != http.StatusNotFound {
			t.Errorf("file %q: status = %d, want 404", file, status)
		}
	}
}

func TestBackfillOnMissingTailedFileIsEmptyAtStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.log") // tailed, not created yet
	ts := httptest.NewServer(server.New(hub.New(10), []string{path}, 500, nil).Handler())
	t.Cleanup(ts.Close)

	status, body := getBefore(t, ts, url.Values{"file": {path}})
	if status != http.StatusOK || len(body.Lines) != 0 || !body.AtStart {
		t.Fatalf("got %d %+v", status, body)
	}
}

func TestBackfillRejectsBadParams(t *testing.T) {
	ts, path := newBackfillServer(t)
	for _, params := range []url.Values{
		{"file": {path}, "offset": {"abc"}},
		{"file": {path}, "limit": {"0"}},
		{"file": {path}, "limit": {"-2"}},
		{"file": {path}, "limit": {"x"}},
	} {
		if status, _ := getBefore(t, ts, params); status != http.StatusBadRequest {
			t.Errorf("params %v: status = %d, want 400", params, status)
		}
	}
}
