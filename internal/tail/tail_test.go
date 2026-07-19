package tail_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shellsecrets/peekastokk/internal/tail"
)

const pollInterval = 10 * time.Millisecond

// start runs a tailer for path in the background and returns its output
// channel. The tailer is stopped via t.Cleanup.
func start(t *testing.T, path string, opts tail.Options) <-chan tail.Line {
	t.Helper()
	if opts.PollInterval == 0 {
		opts.PollInterval = pollInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	ch := make(chan tail.Line, 256)
	go func() {
		defer close(done)
		tail.New(path, opts).Run(ctx, ch)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ch
}

func collect(t *testing.T, ch <-chan tail.Line, n int) []string {
	t.Helper()
	var got []string
	timeout := time.After(5 * time.Second)
	for len(got) < n {
		select {
		case ln := <-ch:
			got = append(got, ln.Text)
		case <-timeout:
			t.Fatalf("timed out waiting for %d lines, got %q", n, got)
		}
	}
	return got
}

func expectQuiet(t *testing.T, ch <-chan tail.Line) {
	t.Helper()
	select {
	case ln := <-ch:
		t.Fatalf("unexpected line %q", ln.Text)
	case <-time.After(20 * pollInterval):
	}
}

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

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAppendedLinesAreEmitted(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "")
	ch := start(t, path, tail.Options{})

	appendTo(t, path, "hello\nwörld\n")
	if got := collect(t, ch, 2); !equal(got, []string{"hello", "wörld"}) {
		t.Fatalf("got %q", got)
	}
}

func TestPartialLinesAreHeldUntilComplete(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "")
	ch := start(t, path, tail.Options{})

	appendTo(t, path, "no newline yet")
	expectQuiet(t, ch)

	appendTo(t, path, " done\n")
	if got := collect(t, ch, 1); got[0] != "no newline yet done" {
		t.Fatalf("got %q", got)
	}
}

func TestCRLFLinesAreTrimmed(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "")
	ch := start(t, path, tail.Options{})

	appendTo(t, path, "windows line\r\n")
	if got := collect(t, ch, 1); got[0] != "windows line" {
		t.Fatalf("got %q", got)
	}
}

func TestReplayIsBoundedAndAlignedToLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "aaaa\nbbbb\ncccc\n")

	// 7 trailing bytes land mid-"bbbb"; the partial line must be skipped.
	ch := start(t, path, tail.Options{TailBytes: 7})
	if got := collect(t, ch, 1); got[0] != "cccc" {
		t.Fatalf("got %q", got)
	}
	expectQuiet(t, ch)
}

func TestNegativeTailBytesStartsAtEnd(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "old\n")

	ch := start(t, path, tail.Options{TailBytes: -1})
	// Let the tailer open the file first: anything written before the first
	// open is deliberately invisible when starting at the end.
	time.Sleep(5 * pollInterval)
	appendTo(t, path, "new\n")
	if got := collect(t, ch, 1); got[0] != "new" {
		t.Fatalf("got %q", got)
	}
}

func TestWaitsForFileToAppear(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "late.log")
	ch := start(t, path, tail.Options{})

	time.Sleep(5 * pollInterval)
	appendTo(t, path, "hi\n")
	if got := collect(t, ch, 1); got[0] != "hi" {
		t.Fatalf("got %q", got)
	}
}

func TestTruncationRestartsFromTop(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "before truncate\n")
	ch := start(t, path, tail.Options{})
	collect(t, ch, 1)

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	// Give the poller a chance to observe the shrunken file before it grows
	// past the old offset again.
	time.Sleep(5 * pollInterval)
	appendTo(t, path, "after truncate\n")
	if got := collect(t, ch, 1); got[0] != "after truncate" {
		t.Fatalf("got %q", got)
	}
}

func TestRotationPicksUpNewFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "in old file\n")
	ch := start(t, path, tail.Options{})
	collect(t, ch, 1)

	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * pollInterval)
	appendTo(t, path, "in new file\n")
	if got := collect(t, ch, 1); got[0] != "in new file" {
		t.Fatalf("got %q", got)
	}
}

func TestLinesCarryByteOffsets(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := make(chan tail.Line, 16)
	go tail.New(path, tail.Options{PollInterval: pollInterval}).Run(ctx, ch)

	appendTo(t, path, "aa\nbbb\n")
	want := []struct {
		text string
		off  int64
	}{{"aa", 0}, {"bbb", 3}}
	for _, w := range want {
		select {
		case ln := <-ch:
			if ln.Text != w.text || ln.Offset != w.off {
				t.Fatalf("got (%q, %d), want (%q, %d)", ln.Text, ln.Offset, w.text, w.off)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out")
		}
	}

	// After truncation offsets restart at zero.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * pollInterval)
	appendTo(t, path, "cc\n")
	select {
	case ln := <-ch:
		if ln.Text != "cc" || ln.Offset != 0 {
			t.Fatalf("got (%q, %d), want (cc, 0)", ln.Text, ln.Offset)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestDockerJSONLogLinesAreUnwrapped(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "docker.log")
	appendTo(t, path, "")
	ch := start(t, path, tail.Options{})

	appendTo(t, path,
		`{"log":"container says hi\n","stream":"stdout","time":"2026-07-18T11:38:07.475856969Z"}`+"\n"+
			`{"log":"and again\n","stream":"stderr","time":"2026-07-18T11:38:08.000000000Z"}`+"\n")
	if got := collect(t, ch, 2); !equal(got, []string{"container says hi", "and again"}) {
		t.Fatalf("got %q", got)
	}
}

func TestDockerJSONLogOffsetsStayRaw(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "docker.log")
	appendTo(t, path, "")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := make(chan tail.Line, 16)
	go tail.New(path, tail.Options{PollInterval: pollInterval}).Run(ctx, ch)

	line1 := `{"log":"first\n","stream":"stdout","time":"2026-07-18T11:38:07Z"}` + "\n"
	line2 := `{"log":"second\n","stream":"stdout","time":"2026-07-18T11:38:08Z"}` + "\n"
	appendTo(t, path, line1+line2)

	want := []struct {
		text string
		off  int64
	}{{"first", 0}, {"second", int64(len(line1))}}
	for _, w := range want {
		select {
		case ln := <-ch:
			if ln.Text != w.text || ln.Offset != w.off {
				t.Fatalf("got (%q, %d), want (%q, %d)", ln.Text, ln.Offset, w.text, w.off)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out")
		}
	}
}

func TestMalformedDockerJSONPassesThrough(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "docker.log")
	appendTo(t, path, "")
	ch := start(t, path, tail.Options{})

	torn := `{"log":"cut off mid`
	appendTo(t, path, torn+"\n"+`{"log":"fine\n","stream":"stdout","time":"2026-07-18T11:38:07Z"}`+"\n")
	got := collect(t, ch, 2)
	if got[0] != torn || got[1] != "fine" {
		t.Fatalf("got %q", got)
	}
}

func TestOversizedLinesAreSplit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "")
	ch := start(t, path, tail.Options{MaxLineBytes: 8})

	appendTo(t, path, strings.Repeat("x", 20)+"\n")
	got := collect(t, ch, 3)
	if joined := strings.Join(got, ""); joined != strings.Repeat("x", 20) {
		t.Fatalf("got %q", got)
	}
	if len(got[0]) != 8 || len(got[1]) != 8 || len(got[2]) != 4 {
		t.Fatalf("unexpected chunk sizes: %d %d %d", len(got[0]), len(got[1]), len(got[2]))
	}
}
