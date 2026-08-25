package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// quiet swallows the duplicate-info logs during tests.
var quiet = slog.New(slog.DiscardHandler)

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(srcs []logSource) []string {
	out := make([]string, len(srcs))
	for i, src := range srcs {
		out[i] = filepath.Base(src.path)
	}
	return out
}

// displayed returns the display names the sources forward and register as.
func displayed(srcs []logSource) []string {
	out := make([]string, len(srcs))
	for i, src := range srcs {
		out[i] = src.name
	}
	return out
}

func TestResolvePathsExpandsDirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "b.log"))
	write(t, filepath.Join(dir, "a.log"))
	write(t, filepath.Join(dir, ".hidden"))
	write(t, filepath.Join(dir, "sub", "nested.log")) // subdir itself must be skipped

	paths, err := resolvePaths(parseLogArgs([]string{dir}), quiet)
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

	paths, err := resolvePaths(parseLogArgs([]string{filepath.Join(dir, "*.log")}), quiet)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(paths); len(got) != 2 || got[0] != "app.log" || got[1] != "worker.log" {
		t.Fatalf("*.log -> %v", got)
	}

	paths, err = resolvePaths(parseLogArgs([]string{filepath.Join(dir, "myproject*")}), quiet)
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

	paths, err := resolvePaths(parseLogArgs([]string{filepath.Join(dir, "*")}), quiet)
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
		if _, err := resolvePaths(parseLogArgs(args), quiet); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestResolvePathsKeepsMissingPlainPathAndDedupes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.log"))

	// A future file is kept; the same file via dir and explicitly is deduped.
	paths, err := resolvePaths(parseLogArgs([]string{
		filepath.Join(dir, "future.log"),
		dir,
		filepath.Join(dir, "a.log"),
	}), quiet)
	if err != nil {
		t.Fatal(err)
	}
	got := names(paths)
	if len(got) != 2 || got[0] != "future.log" || got[1] != "a.log" {
		t.Fatalf("got %v, want [future.log a.log]", got)
	}
}

func TestResolvePathsLogsDuplicates(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.log"))
	write(t, filepath.Join(dir, "b.log"))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	// a.log arrives via the directory, explicitly, and through a glob; the
	// glob adds nothing new at all.
	paths, err := resolvePaths(parseLogArgs([]string{
		dir,
		filepath.Join(dir, "a.log"),
		filepath.Join(dir, "*.log"),
	}), logger)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %v", paths)
	}
	logs := buf.String()
	if !strings.Contains(logs, "skipping duplicate log file") {
		t.Errorf("duplicate files were not logged:\n%s", logs)
	}
	if !strings.Contains(logs, "argument only named files that are already tailed") {
		t.Errorf("fully-duplicate argument was not logged:\n%s", logs)
	}
	if strings.Count(logs, "skipping duplicate log file") != 3 {
		t.Errorf("want 3 duplicate lines (a.log explicit + a.log,b.log via glob), got:\n%s", logs)
	}
}

func TestResolvePathsDedupesSymlinkAliases(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.log")
	write(t, target)
	link := filepath.Join(dir, "alias.log")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	paths, err := resolvePaths(parseLogArgs([]string{target, link}), logger)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0].path != target {
		t.Fatalf("paths = %v, want just the target", paths)
	}
	if !strings.Contains(buf.String(), "already_tailed_as") {
		t.Errorf("symlink duplicate not logged with its target:\n%s", buf.String())
	}
}

func TestResolvePathsCapsFileCount(t *testing.T) {
	dir := t.TempDir()
	args := make([]string, maxTailedFiles+1)
	for i := range args {
		args[i] = filepath.Join(dir, "missing-", strings.Repeat("x", 3), "-", string(rune('a'+i%26)), "-", filepath.Base(t.TempDir()))
	}
	if _, err := resolvePaths(parseLogArgs(args), quiet); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("expected cap error, got %v", err)
	}
}

func TestSplitWatchArgs(t *testing.T) {
	dir := t.TempDir()
	plainFile := filepath.Join(dir, "app.log")
	write(t, plainFile)
	missing := filepath.Join(dir, "not-yet.log") // plain path that may appear later
	glob := filepath.Join(dir, "*.log")

	plain, watch := splitWatchArgs(parseLogArgs([]string{plainFile, dir, glob, missing}))
	if len(plain) != 2 || plain[0].spec != plainFile || plain[1].spec != missing {
		t.Errorf("plain = %v", plain)
	}
	if len(watch) != 2 || watch[0].spec != dir || watch[1].spec != glob {
		t.Errorf("watch = %v", watch)
	}
}

func TestScanWatchArgsTracksCreationAndDeletion(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	write(t, a)

	if got := names(scanWatchArgs(parseLogArgs([]string{dir}), nil, maxTailedFiles, quiet)); len(got) != 1 || got[0] != "a.log" {
		t.Fatalf("initial scan = %v", got)
	}

	b := filepath.Join(dir, "b.log")
	write(t, b)
	if got := names(scanWatchArgs(parseLogArgs([]string{dir}), nil, maxTailedFiles, quiet)); len(got) != 2 {
		t.Fatalf("after creation = %v", got)
	}

	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	if got := names(scanWatchArgs(parseLogArgs([]string{dir}), nil, maxTailedFiles, quiet)); len(got) != 1 || got[0] != "b.log" {
		t.Fatalf("after deletion = %v", got)
	}
}

func TestScanWatchArgsSkipsExcludedAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	b := filepath.Join(dir, "b.log")
	write(t, a)
	write(t, b)

	id, err := pathIdentity(a)
	if err != nil {
		t.Fatal(err)
	}
	// The directory and the overlapping glob both name b.log; a.log is
	// already tailed statically.
	got := scanWatchArgs(parseLogArgs([]string{dir, filepath.Join(dir, "*.log")}),
		map[string]bool{id: true}, maxTailedFiles, quiet)
	if len(got) != 1 || got[0].path != b {
		t.Fatalf("scan = %v, want just %s", got, b)
	}
}

func TestScanWatchArgsToleratesEmptinessAndCapsAtLimit(t *testing.T) {
	if got := scanWatchArgs(parseLogArgs([]string{t.TempDir(), "/nonexistent/*.log"}), nil, maxTailedFiles, quiet); len(got) != 0 {
		t.Fatalf("empty scan = %v", got)
	}

	dir := t.TempDir()
	for _, n := range []string{"a.log", "b.log", "c.log"} {
		write(t, filepath.Join(dir, n))
	}
	if got := scanWatchArgs(parseLogArgs([]string{dir}), nil, 2, quiet); len(got) != 2 {
		t.Fatalf("capped scan = %v, want 2 files", got)
	}
}

func TestResolvePathsAppliesAliases(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "app.log"))
	write(t, filepath.Join(dir, "error.log"))
	plain := filepath.Join(t.TempDir(), "single.log")
	write(t, plain)

	// A plain file takes the alias as its whole name; a directory or glob
	// prefixes it onto each expanded file's base name.
	sources, err := resolvePaths(parseLogArgs([]string{
		plain + ":web",
		dir + ":worker1",
	}), quiet)
	if err != nil {
		t.Fatal(err)
	}
	got := displayed(sources)
	want := []string{"web", "worker1/app.log", "worker1/error.log"}
	if len(got) != len(want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %v, want %v", got, want)
		}
	}

	// Without an alias the base name is unchanged.
	sources, err = resolvePaths(parseLogArgs([]string{plain}), quiet)
	if err != nil {
		t.Fatal(err)
	}
	if got := displayed(sources); len(got) != 1 || got[0] != "single.log" {
		t.Fatalf("unaliased names = %v", got)
	}
}

func TestScanWatchArgsAppliesAliases(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.log"))
	write(t, filepath.Join(dir, "notes.txt"))

	got := displayed(scanWatchArgs(parseLogArgs([]string{
		filepath.Join(dir, "*.log") + ":worker2",
	}), nil, maxTailedFiles, quiet))
	if len(got) != 1 || got[0] != "worker2/a.log" {
		t.Fatalf("names = %v, want [worker2/a.log]", got)
	}
}
