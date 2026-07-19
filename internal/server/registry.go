package server

import (
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
)

// entryKind distinguishes a disk-backed local file from a forwarded
// source with no local backing (which /api/before cannot scroll into).
type entryKind int

const (
	entryLocal entryKind = iota
	entryForwarded
)

// registry is the concurrency-safe bookkeeping of every known source.
// Entries were once fixed at construction; forwarded sources now register
// at runtime, after the mux is already serving, so every access locks.
// Entries are append-only for the process lifetime: an opaque id handed
// to a browser is never reused for a different source — a disconnected
// forwarder's entries just go quiet, like a local file whose producer
// stopped writing.
type registry struct {
	mu            sync.RWMutex
	entries       []fileEntry
	bases         []string          // pre-dedup base names, aligned with entries
	pathByID      map[string]string // opaque id -> key (path or virtual key)
	idByPath      map[string]string
	kindByID      map[string]entryKind
	nextID        int
	forwardCounts map[string]int // clientName -> distinct forwarded sources
}

func newRegistry() *registry {
	return &registry{
		pathByID:      make(map[string]string),
		idByPath:      make(map[string]string),
		kindByID:      make(map[string]entryKind),
		forwardCounts: make(map[string]int),
	}
}

// register adds key with the given display base name, or returns the
// existing id if key is already known. Display names are re-deduplicated
// over the full accumulated set on every addition, so the "#2"-suffix
// convention applies uniformly across local and forwarded entries.
func (rg *registry) register(key, baseName string, kind entryKind) (id string, isNew bool) {
	rg.mu.Lock()
	defer rg.mu.Unlock()

	if id, ok := rg.idByPath[key]; ok {
		return id, false
	}
	id = strconv.Itoa(rg.nextID)
	rg.nextID++
	rg.bases = append(rg.bases, baseName)
	rg.entries = append(rg.entries, fileEntry{ID: id})
	names := dedupeNames(rg.bases)
	for i := range rg.entries {
		rg.entries[i].Name = names[i]
	}
	rg.pathByID[id] = key
	rg.idByPath[key] = id
	rg.kindByID[id] = kind
	return id, true
}

// snapshot returns a copy of the entries for JSON responses.
func (rg *registry) snapshot() []fileEntry {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	out := make([]fileEntry, len(rg.entries))
	copy(out, rg.entries)
	return out
}

// lookupPath resolves an opaque id to its key and kind.
func (rg *registry) lookupPath(id string) (key string, kind entryKind, ok bool) {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	key, ok = rg.pathByID[id]
	if ok {
		kind = rg.kindByID[id]
	}
	return key, kind, ok
}

// lookupID resolves a key (path or virtual key) to its opaque id.
func (rg *registry) lookupID(key string) (string, bool) {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	id, ok := rg.idByPath[key]
	return id, ok
}

// dedupeNames numbers duplicate names ("app.log #2") so identically-named
// sources stay distinguishable.
func dedupeNames(bases []string) []string {
	total := make(map[string]int, len(bases))
	for _, b := range bases {
		total[b]++
	}
	seen := make(map[string]int, len(bases))
	names := make([]string, len(bases))
	for i, b := range bases {
		if total[b] > 1 {
			seen[b]++
			names[i] = fmt.Sprintf("%s #%d", b, seen[b])
		} else {
			names[i] = b
		}
	}
	return names
}

// displayNames maps paths to their deduplicated base names, without
// revealing the directories they came from.
func displayNames(paths []string) []string {
	bases := make([]string, len(paths))
	for i, p := range paths {
		bases[i] = filepath.Base(p)
	}
	return dedupeNames(bases)
}
