package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shellsecrets/peekastokk/internal/hub"
	"github.com/shellsecrets/peekastokk/internal/server"
	"github.com/shellsecrets/peekastokk/internal/tail"
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
	ts := httptest.NewServer(server.New(hub.New(10), server.Options{Files: []string{path}, Lines: 500}).Handler())
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
	ts, _ := newBackfillServer(t)

	// Anchor at line-5 (offset 35): the 3 lines before it are 2, 3, 4.
	status, body := getBefore(t, ts, url.Values{
		"file": {"0"}, "offset": {"35"}, "limit": {"3"},
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
		"file": {"0"}, "offset": {"14"}, "limit": {"5"},
	})
	if status != http.StatusOK || len(body.Lines) != 2 || !body.AtStart {
		t.Fatalf("second page = %d %+v", status, body)
	}
	if body.Lines[0].Off != 0 || body.Lines[0].Text != "line-0" {
		t.Fatalf("lines[0] = %+v", body.Lines[0])
	}
}

func TestBackfillWithoutOffsetAnchorsAtEOF(t *testing.T) {
	ts, _ := newBackfillServer(t)
	status, body := getBefore(t, ts, url.Values{"file": {"0"}})
	if status != http.StatusOK || len(body.Lines) != 10 || !body.AtStart {
		t.Fatalf("got %d, %d lines, atStart=%v", status, len(body.Lines), body.AtStart)
	}
	if body.Lines[9].Text != "line-9" {
		t.Fatalf("last = %+v", body.Lines[9])
	}
}

func TestBackfillAtOffsetZeroIsEmptyAndAtStart(t *testing.T) {
	ts, _ := newBackfillServer(t)
	status, body := getBefore(t, ts, url.Values{"file": {"0"}, "offset": {"0"}})
	if status != http.StatusOK || len(body.Lines) != 0 || !body.AtStart {
		t.Fatalf("got %d %+v", status, body)
	}
}

func TestBackfillDropsUnterminatedTrailingLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.log")
	if err := os.WriteFile(path, []byte("done\nno newline yet"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(hub.New(10), server.Options{Files: []string{path}, Lines: 500}).Handler())
	t.Cleanup(ts.Close)

	status, body := getBefore(t, ts, url.Values{"file": {"0"}})
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
	ts := httptest.NewServer(server.New(hub.New(10), server.Options{Files: []string{path}, Lines: 500}).Handler())
	t.Cleanup(ts.Close)

	status, body := getBefore(t, ts, url.Values{"file": {"0"}})
	if status != http.StatusOK || len(body.Lines) != 0 || !body.AtStart {
		t.Fatalf("got %d %+v", status, body)
	}
}

// dockerLine builds one Docker json-file envelope line (with trailing \n).
func dockerLine(text string) string {
	return `{"log":"` + text + `\n","stream":"stdout","time":"2026-07-18T11:38:07.475856969Z"}` + "\n"
}

func TestBackfillUnwrapsDockerJSONLogLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docker.log")
	l1, l2, l3 := dockerLine("alpha"), dockerLine("beta"), dockerLine("gamma")
	if err := os.WriteFile(path, []byte(l1+l2+l3), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(hub.New(10), server.Options{Files: []string{path}, Lines: 500}).Handler())
	t.Cleanup(ts.Close)

	status, body := getBefore(t, ts, url.Values{"file": {"0"}})
	if status != http.StatusOK || len(body.Lines) != 3 {
		t.Fatalf("got %d, %d lines", status, len(body.Lines))
	}
	wantTexts := []string{"alpha", "beta", "gamma"}
	wantOffs := []int64{0, int64(len(l1)), int64(len(l1) + len(l2))}
	for i := range body.Lines {
		if body.Lines[i].Text != wantTexts[i] || body.Lines[i].Off != wantOffs[i] {
			t.Errorf("lines[%d] = (%q, %d), want (%q, %d)",
				i, body.Lines[i].Text, body.Lines[i].Off, wantTexts[i], wantOffs[i])
		}
	}
}

func TestBackfillDockerJSONLogPaginatesAcrossPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docker.log")
	var content string
	var offs []int64
	for i := 0; i < 10; i++ {
		offs = append(offs, int64(len(content)))
		content += dockerLine(fmt.Sprintf("event-%d", i))
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(hub.New(10), server.Options{Files: []string{path}, Lines: 500}).Handler())
	t.Cleanup(ts.Close)

	// Page backwards 3 lines at a time from EOF; every page must be
	// contiguous and correctly unwrapped.
	anchor := int64(len(content))
	seen := 0
	for {
		status, body := getBefore(t, ts, url.Values{
			"file": {"0"}, "offset": {fmt.Sprint(anchor)}, "limit": {"3"},
		})
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		if len(body.Lines) == 0 {
			break
		}
		for i := len(body.Lines) - 1; i >= 0; i-- {
			seen++
			wantIdx := 10 - seen
			if body.Lines[i].Text != fmt.Sprintf("event-%d", wantIdx) || body.Lines[i].Off != offs[wantIdx] {
				t.Fatalf("page line = (%q, %d), want (event-%d, %d)",
					body.Lines[i].Text, body.Lines[i].Off, wantIdx, offs[wantIdx])
			}
		}
		anchor = body.Lines[0].Off
		if body.AtStart {
			break
		}
	}
	if seen != 10 {
		t.Fatalf("paged through %d lines, want 10", seen)
	}
}

// TestLiveAndBackfillAgreeOnDockerUnwrap proves the two independent code
// paths (live tailer, disk backfill) produce identical text for the same
// docker-json file.
func TestLiveAndBackfillAgreeOnDockerUnwrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docker.log")
	content := dockerLine("one") + `{"log":"torn` + "\n" + dockerLine("three")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Live path.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan tail.Line, 16)
	go tail.New(path, tail.Options{PollInterval: 10 * time.Millisecond}).Run(ctx, ch)
	var live []string
	for len(live) < 3 {
		select {
		case ln := <-ch:
			live = append(live, ln.Text)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out, got %q", live)
		}
	}

	// Backfill path.
	ts := httptest.NewServer(server.New(hub.New(10), server.Options{Files: []string{path}, Lines: 500}).Handler())
	t.Cleanup(ts.Close)
	status, body := getBefore(t, ts, url.Values{"file": {"0"}})
	if status != http.StatusOK || len(body.Lines) != 3 {
		t.Fatalf("backfill got %d, %d lines", status, len(body.Lines))
	}
	for i := range live {
		if body.Lines[i].Text != live[i] {
			t.Errorf("line %d: backfill %q != live %q", i, body.Lines[i].Text, live[i])
		}
	}
}

func TestBackfillRejectsBadParams(t *testing.T) {
	ts, _ := newBackfillServer(t)
	for _, params := range []url.Values{
		{"file": {"0"}, "offset": {"abc"}},
		{"file": {"0"}, "limit": {"0"}},
		{"file": {"0"}, "limit": {"-2"}},
		{"file": {"0"}, "limit": {"x"}},
	} {
		if status, _ := getBefore(t, ts, params); status != http.StatusBadRequest {
			t.Errorf("params %v: status = %d, want 400", params, status)
		}
	}
}
