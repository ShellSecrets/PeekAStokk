package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shellsecrets/peekastokk/internal/config"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// isolateHome points HOME (and clears XDG_CONFIG_HOME) at a temp dir so
// tests never see the developer's real config.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

func TestLoadParsesAllKeys(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	writeFile(t, path, `
# global settings
addr      = 0.0.0.0:9000
history   = 5000
lines     = 750
poll      = 100ms
tail_bytes = -1            # underscores and trailing comments are fine
max-line-bytes = 1024
log-level = "debug"

file = /var/log/app.log
file = relative/worker.log
file = ~/logs/api.log
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "0.0.0.0:9000" || !cfg.Has("addr") {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.History != 5000 || cfg.Poll != 100*time.Millisecond {
		t.Errorf("History = %d, Poll = %v", cfg.History, cfg.Poll)
	}
	if cfg.Lines != 750 || !cfg.Has("lines") {
		t.Errorf("Lines = %d", cfg.Lines)
	}
	if cfg.TailBytes != -1 || cfg.MaxLineBytes != 1024 || cfg.LogLevel != "debug" {
		t.Errorf("TailBytes = %d, MaxLineBytes = %d, LogLevel = %q",
			cfg.TailBytes, cfg.MaxLineBytes, cfg.LogLevel)
	}
	home, _ := os.UserHomeDir()
	want := []string{
		"/var/log/app.log",
		filepath.Join(dir, "relative/worker.log"),
		filepath.Join(home, "logs/api.log"),
	}
	if len(cfg.Files) != 3 {
		t.Fatalf("Files = %v", cfg.Files)
	}
	for i, w := range want {
		if cfg.Files[i] != w {
			t.Errorf("Files[%d] = %q, want %q", i, cfg.Files[i], w)
		}
	}
	if cfg.Has("port") {
		t.Error("port should be absent")
	}
}

func TestLoadPort(t *testing.T) {
	isolateHome(t)
	path := filepath.Join(t.TempDir(), "config")
	writeFile(t, path, "port = 9000\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Has("port") || cfg.Port != 9000 {
		t.Errorf("Port = %d", cfg.Port)
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	isolateHome(t)
	cases := map[string]string{
		"unknown key":      "bogus = 1\n",
		"missing equals":   "addr 127.0.0.1:1\n",
		"bad port":         "port = 99999\n",
		"bad history":      "history = -1\n",
		"bad lines":        "lines = 0\n",
		"bad poll":         "poll = fast\n",
		"bad log level":    "log-level = loud\n",
		"auth no colon":    "auth = devonly\n",
		"auth empty user":  "auth = :pass\n",
		"auth empty pass":  "auth = dev:\n",
		"duplicate scalar": "port = 1\nport = 2\n",
		"addr and port":    "addr = :9000\nport = 9000\n",
		"empty file value": "file =\n",
		"unterminated q":   "addr = \"oops\n",
		"max-line zero":    "max-line-bytes = 0\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			writeFile(t, path, content)
			if _, err := config.Load(path); err == nil {
				t.Fatalf("expected error for %q", content)
			}
		})
	}
}

func TestLoadAuth(t *testing.T) {
	isolateHome(t)
	path := filepath.Join(t.TempDir(), "config")
	writeFile(t, path, "auth = dev:s3cret\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Has("auth") || cfg.Auth != "dev:s3cret" {
		t.Errorf("Auth = %q", cfg.Auth)
	}
}

func TestDuplicateFileKeyIsAllowed(t *testing.T) {
	isolateHome(t)
	path := filepath.Join(t.TempDir(), "config")
	writeFile(t, path, "file = /a.log\nfile = /b.log\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Files) != 2 {
		t.Fatalf("Files = %v", cfg.Files)
	}
}

func TestQuotedValuePreservesHash(t *testing.T) {
	isolateHome(t)
	path := filepath.Join(t.TempDir(), "config")
	writeFile(t, path, `file = "/var/log/app #1.log"`+"\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Files[0] != "/var/log/app #1.log" {
		t.Fatalf("Files[0] = %q", cfg.Files[0])
	}
}

func TestLoadExplicitMissingPathIsError(t *testing.T) {
	isolateHome(t)
	if _, err := config.Load(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadNoConfigAnywhereIsNil(t *testing.T) {
	isolateHome(t)
	cfg, err := config.Load("")
	if err != nil || cfg != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", cfg, err)
	}
}

func TestSearchOrder(t *testing.T) {
	home := isolateHome(t)
	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)

	xdgConfig := filepath.Join(xdg, "peekastokk", "config")
	dotFile := filepath.Join(home, ".peekastokk")
	dotDirConfig := filepath.Join(home, ".peekastokk", "config")

	// Lowest priority first: ~/.peekastokk as a directory with a config.
	writeFile(t, dotDirConfig, "port = 3\n")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 3 {
		t.Fatalf("Port = %d, want 3 (~/.peekastokk/config)", cfg.Port)
	}

	// ~/.peekastokk as a plain file beats the directory form. It cannot
	// coexist with the directory, so use a fresh home.
	home2 := t.TempDir()
	t.Setenv("HOME", home2)
	t.Setenv("XDG_CONFIG_HOME", "")
	writeFile(t, filepath.Join(home2, ".peekastokk"), "port = 2\n")
	if cfg, err = config.Load(""); err != nil || cfg.Port != 2 {
		t.Fatalf("(%+v, %v), want Port 2 (~/.peekastokk)", cfg, err)
	}

	// XDG beats everything.
	t.Setenv("XDG_CONFIG_HOME", xdg)
	writeFile(t, xdgConfig, "port = 1\n")
	if cfg, err = config.Load(""); err != nil || cfg.Port != 1 {
		t.Fatalf("(%+v, %v), want Port 1 (XDG)", cfg, err)
	}
	_ = dotFile
}

func TestXDGDefaultsToDotConfig(t *testing.T) {
	home := isolateHome(t)
	writeFile(t, filepath.Join(home, ".config", "peekastokk", "config"), "port = 7\n")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.Port != 7 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if !strings.HasPrefix(cfg.Path, home) {
		t.Fatalf("Path = %q", cfg.Path)
	}
}
