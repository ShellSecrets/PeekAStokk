package config_test

import (
	"path/filepath"
	"testing"

	"github.com/shellsecrets/peekastokk/internal/config"
)

// TestRepoExampleConfigIsValid keeps config.example at the repository root
// parseable, so the shipped example can never drift out of sync with the
// parser.
func TestRepoExampleConfigIsValid(t *testing.T) {
	isolateHome(t)
	path, err := filepath.Abs(filepath.Join("..", "..", "config.example"))
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8844 {
		t.Errorf("Port = %d, want 8844", cfg.Port)
	}
	if len(cfg.Files) < 1 {
		t.Error("example should tail at least one file")
	}
	for _, key := range []string{"history", "lines", "poll", "tail-bytes", "max-line-bytes", "log-level"} {
		if !cfg.Has(key) {
			t.Errorf("example should demonstrate %q", key)
		}
	}
}
