package main

import (
	"path/filepath"
	"sync"
)

// sourceNamer maps a tailed path to the display name it forwards and
// registers as: a Docker container's alias/resolved name, or a plain
// file's base name. The Docker watcher updates it on every scan; the
// per-line consumer reads it on every forwarded line.
type sourceNamer struct {
	mu    sync.RWMutex
	names map[string]string
}

func newSourceNamer() *sourceNamer {
	return &sourceNamer{names: make(map[string]string)}
}

func (n *sourceNamer) set(path, name string) {
	n.mu.Lock()
	n.names[path] = name
	n.mu.Unlock()
}

func (n *sourceNamer) delete(path string) {
	n.mu.Lock()
	delete(n.names, path)
	n.mu.Unlock()
}

// name returns the display name for path, falling back to its base name
// (e.g. when the watcher raced ahead of a container's removal).
func (n *sourceNamer) name(path string) string {
	n.mu.RLock()
	v, ok := n.names[path]
	n.mu.RUnlock()
	if ok {
		return v
	}
	return filepath.Base(path)
}

// all returns every registered display name, for announcing this client's
// sources to the server it forwards to.
func (n *sourceNamer) all() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	names := make([]string, 0, len(n.names))
	for _, v := range n.names {
		names = append(names, v)
	}
	return names
}

// pathOf is the reverse lookup: which local file a display name refers
// to. Used to answer the server's remote scrollback requests — only names
// this process itself registered ever resolve. Linear scan over a small,
// bounded set.
func (n *sourceNamer) pathOf(name string) (string, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	for p, v := range n.names {
		if v == name {
			return p, true
		}
	}
	return "", false
}
