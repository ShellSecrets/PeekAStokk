// Package dockerwatch discovers Docker containers by reading the daemon's
// own on-disk state (a read-only view of <data-root>/containers), with no
// Docker socket access at all: the container id is the directory name and
// the json-file log sits inside it. Polling a directory listing matches
// the tailer's own no-fsnotify philosophy and tracks containers starting
// and stopping over time.
package dockerwatch

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultRoot is where Docker's json-file logs live under the default
// data-root; override via docker-root when --data-root or rootless Docker
// moved it (docker info --format '{{.DockerRootDir}}' + "/containers").
const DefaultRoot = "/var/lib/docker/containers"

// DefaultPoll is the container re-scan interval; container churn is far
// less frequent than log lines, so this is deliberately slower than the
// log poll.
const DefaultPoll = 2 * time.Second

// Container is one discovered, selected container.
type Container struct {
	ID          string
	LogPath     string
	DisplayName string // alias > resolved name > short id
}

// Watcher scans a containers directory against a Selector.
type Watcher struct {
	root     string
	sel      *Selector
	log      *slog.Logger
	fallback map[string]bool // ids whose name resolution failure was logged
}

// NewWatcher builds a Watcher over root (DefaultRoot when empty).
func NewWatcher(root string, sel *Selector, logger *slog.Logger) *Watcher {
	if root == "" {
		root = DefaultRoot
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Watcher{root: root, sel: sel, log: logger, fallback: make(map[string]bool)}
}

// configV2 is the minimal slice of Docker's internal per-container state
// file we read for a friendly name. The file is not a stable public API;
// any parse failure falls back to the short container id.
type configV2 struct {
	Name string `json:"Name"`
}

// resolveName returns the container's friendly name, or the short id when
// the name cannot be resolved.
func (w *Watcher) resolveName(id string) string {
	short := id
	if len(short) > 12 {
		short = short[:12]
	}
	data, err := os.ReadFile(filepath.Join(w.root, id, "config.v2.json"))
	if err != nil {
		w.noteFallback(id, err)
		return short
	}
	var cfg configV2
	if err := json.Unmarshal(data, &cfg); err != nil {
		w.noteFallback(id, err)
		return short
	}
	name := strings.TrimPrefix(cfg.Name, "/")
	if name == "" {
		w.noteFallback(id, nil)
		return short
	}
	return name
}

// noteFallback logs a name-resolution fallback once per container, at
// debug, so Docker schema drift is discoverable without per-poll noise.
func (w *Watcher) noteFallback(id string, err error) {
	if w.fallback[id] {
		return
	}
	w.fallback[id] = true
	w.log.Debug("container name unresolved; using short id", "container", id, "error", err)
}

// Scan lists the containers directory and returns the selected containers
// with their resolved display names. A missing root directory is not an
// error (the bind mount may appear later); it yields an empty result.
func (w *Watcher) Scan() []Container {
	entries, err := os.ReadDir(w.root)
	if err != nil {
		w.log.Debug("docker root unreadable", "root", w.root, "error", err)
		return nil
	}
	var out []Container
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		logPath := filepath.Join(w.root, id, id+"-json.log")
		if _, err := os.Stat(logPath); err != nil {
			continue // not a container dir, or no json-file log driver
		}
		name := w.resolveName(id)
		alias, ok := w.sel.Match(name, id)
		if !ok {
			continue
		}
		display := name
		if alias != "" {
			display = alias
		}
		out = append(out, Container{ID: id, LogPath: logPath, DisplayName: display})
	}
	return out
}

// Reconciler receives each scan's result; it is how the watcher drives a
// tailgroup and a name registry without importing either.
type Reconciler func(containers []Container)

// Run re-scans on every tick until ctx is done, invoking reconcile with
// each result (including the initial immediate scan).
func Run(ctx context.Context, w *Watcher, poll time.Duration, reconcile Reconciler) {
	if poll <= 0 {
		poll = DefaultPoll
	}
	reconcile(w.Scan())
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile(w.Scan())
		}
	}
}
