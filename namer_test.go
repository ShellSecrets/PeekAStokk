package main

import (
	"fmt"
	"sync"
	"testing"
)

func TestSourceNamerLookupAndFallback(t *testing.T) {
	n := newSourceNamer()

	// Unknown path falls back to its base name.
	if got := n.name("/var/log/app.log"); got != "app.log" {
		t.Fatalf("fallback = %q, want app.log", got)
	}

	n.set("/var/lib/docker/containers/abc/abc-json.log", "nginx")
	if got := n.name("/var/lib/docker/containers/abc/abc-json.log"); got != "nginx" {
		t.Fatalf("mapped = %q, want nginx", got)
	}

	// Deleting restores the base-name fallback (the watcher racing ahead
	// of a container's removal).
	n.delete("/var/lib/docker/containers/abc/abc-json.log")
	if got := n.name("/var/lib/docker/containers/abc/abc-json.log"); got != "abc-json.log" {
		t.Fatalf("after delete = %q, want abc-json.log", got)
	}
}

// TestSourceNamerConcurrent exercises the RWMutex under -race: the Docker
// watcher writes while the per-line consumer reads.
func TestSourceNamerConcurrent(t *testing.T) {
	n := newSourceNamer()
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range 200 {
				p := fmt.Sprintf("/logs/%d.log", i%20)
				switch (w + i) % 3 {
				case 0:
					n.set(p, fmt.Sprintf("name-%d", i))
				case 1:
					n.name(p)
				default:
					n.delete(p)
				}
			}
		}(w)
	}
	wg.Wait()
}
