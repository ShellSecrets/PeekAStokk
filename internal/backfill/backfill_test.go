package backfill_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/shellsecrets/peekastokk/internal/backfill"
)

func writeLog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadLinesBeforePagesBackwards(t *testing.T) {
	var content string
	var offs []int64
	for i := range 10 {
		offs = append(offs, int64(len(content)))
		content += fmt.Sprintf("line-%d\n", i)
	}
	path := writeLog(t, content)

	// Anchor at line-5's offset: the 3 lines before are 2, 3, 4.
	lines, atStart, err := backfill.ReadLinesBefore(path, offs[5], 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || atStart {
		t.Fatalf("got %d lines atStart=%v", len(lines), atStart)
	}
	for i, want := range []int{2, 3, 4} {
		if lines[i].Text != fmt.Sprintf("line-%d", want) || lines[i].Off != offs[want] {
			t.Fatalf("lines[%d] = %+v", i, lines[i])
		}
	}

	// Negative offset anchors at EOF and atStart triggers at the top.
	lines, atStart, err = backfill.ReadLinesBefore(path, -1, 100)
	if err != nil || len(lines) != 10 || !atStart {
		t.Fatalf("EOF anchor: %d lines atStart=%v err=%v", len(lines), atStart, err)
	}
}

func TestReadLinesBeforeEdges(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if _, _, err := backfill.ReadLinesBefore(filepath.Join(t.TempDir(), "nope"), -1, 10); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("offset zero", func(t *testing.T) {
		lines, atStart, err := backfill.ReadLinesBefore(writeLog(t, "a\nb\n"), 0, 10)
		if err != nil || len(lines) != 0 || !atStart {
			t.Fatalf("got %v %v %v", lines, atStart, err)
		}
	})
	t.Run("unterminated trailing line skipped", func(t *testing.T) {
		lines, _, err := backfill.ReadLinesBefore(writeLog(t, "done\nno newline"), -1, 10)
		if err != nil || len(lines) != 1 || lines[0].Text != "done" {
			t.Fatalf("got %+v err=%v", lines, err)
		}
	})
	t.Run("limit caps and keeps newest", func(t *testing.T) {
		lines, atStart, err := backfill.ReadLinesBefore(writeLog(t, "a\nb\nc\nd\n"), -1, 2)
		if err != nil || len(lines) != 2 || atStart {
			t.Fatalf("got %+v atStart=%v err=%v", lines, atStart, err)
		}
		if lines[0].Text != "c" || lines[1].Text != "d" {
			t.Fatalf("kept %q,%q, want newest c,d", lines[0].Text, lines[1].Text)
		}
	})
	t.Run("docker json unwrapped", func(t *testing.T) {
		raw := `{"log":"inside text\n","stream":"stdout","time":"2026-07-19T06:00:00Z"}` + "\n"
		lines, _, err := backfill.ReadLinesBefore(writeLog(t, raw), -1, 10)
		if err != nil || len(lines) != 1 || lines[0].Text != "inside text" {
			t.Fatalf("got %+v err=%v", lines, err)
		}
		if lines[0].Off != 0 {
			t.Fatalf("offset must stay raw, got %d", lines[0].Off)
		}
	})
}
