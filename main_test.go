package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}

func TestResolvePathsExpandsDirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "b.log"))
	write(t, filepath.Join(dir, "a.log"))
	write(t, filepath.Join(dir, ".hidden"))
	write(t, filepath.Join(dir, "sub", "nested.log")) // subdir itself must be skipped

	paths, err := resolvePaths([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	got := names(paths)
	if len(got) != 2 || got[0] != "a.log" || got[1] != "b.log" {
		t.Fatalf("got %v, want [a.log b.log]", got)
	}
}

func TestResolvePathsExpandsGlobs(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "app.log"))
	write(t, filepath.Join(dir, "worker.log"))
	write(t, filepath.Join(dir, "notes.txt"))
	write(t, filepath.Join(dir, "myproject-a.out"))
	write(t, filepath.Join(dir, "myproject-b.out"))

	paths, err := resolvePaths([]string{filepath.Join(dir, "*.log")})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(paths); len(got) != 2 || got[0] != "app.log" || got[1] != "worker.log" {
		t.Fatalf("*.log -> %v", got)
	}

	paths, err = resolvePaths([]string{filepath.Join(dir, "myproject*")})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(paths); len(got) != 2 || got[0] != "myproject-a.out" {
		t.Fatalf("myproject* -> %v", got)
	}
}

func TestResolvePathsGlobSkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.log"))
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	paths, err := resolvePaths([]string{filepath.Join(dir, "*")})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(paths); len(got) != 1 || got[0] != "a.log" {
		t.Fatalf("got %v, want [a.log]", got)
	}
}

func TestResolvePathsErrors(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"unmatched pattern": {filepath.Join(dir, "*.nope")},
		"empty directory":   {empty},
		"no args":           {},
	} {
		if _, err := resolvePaths(args); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestResolvePathsKeepsMissingPlainPathAndDedupes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.log"))

	// A future file is kept; the same file via dir and explicitly is deduped.
	paths, err := resolvePaths([]string{
		filepath.Join(dir, "future.log"),
		dir,
		filepath.Join(dir, "a.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := names(paths)
	if len(got) != 2 || got[0] != "future.log" || got[1] != "a.log" {
		t.Fatalf("got %v, want [future.log a.log]", got)
	}
}

func TestResolvePathsCapsFileCount(t *testing.T) {
	dir := t.TempDir()
	args := make([]string, maxTailedFiles+1)
	for i := range args {
		args[i] = filepath.Join(dir, "missing-", strings.Repeat("x", 3), "-", string(rune('a'+i%26)), "-", filepath.Base(t.TempDir()))
	}
	if _, err := resolvePaths(args); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("expected cap error, got %v", err)
	}
}
