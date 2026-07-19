package dockerwatch

import (
	"fmt"
	"path/filepath"
	"strings"
)

// entrySel is one parsed docker-containers entry.
type entrySel struct {
	pattern string // exact name/ID, glob, or "*"
	alias   string // display alias; only ever set on an exact entry
	isGlob  bool
	isAll   bool
}

// Selector decides which containers are tailed and what an exact match is
// displayed as.
type Selector struct {
	entries []entrySel
}

// NewSelector parses docker-containers entries. Three forms are accepted:
//
//	name-or-id[:alias]   exact match, optionally displayed as alias
//	pattern*             glob (filepath.Match), matches many, no alias
//	*                    all containers, no alias
//
// An alias on a glob or "*" is rejected: those match a changing set of
// containers, which one static name cannot describe. An empty entries
// list selects all containers (docker = true alone means everything).
func NewSelector(entries []string) (*Selector, error) {
	s := &Selector{}
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			return nil, fmt.Errorf("empty docker-containers entry")
		}
		pattern, alias, hasAlias := strings.Cut(e, ":")
		if hasAlias && (pattern == "" || alias == "") {
			return nil, fmt.Errorf("docker-containers %q: both name and alias must be non-empty", e)
		}
		isAll := pattern == "*"
		isGlob := !isAll && strings.ContainsAny(pattern, "*?[")
		if hasAlias && (isAll || isGlob) {
			return nil, fmt.Errorf("docker-containers %q: an alias is only valid on an exact container name or id, not a pattern", e)
		}
		if isGlob {
			if _, err := filepath.Match(pattern, ""); err != nil {
				return nil, fmt.Errorf("docker-containers %q: invalid pattern: %v", e, err)
			}
		}
		s.entries = append(s.entries, entrySel{pattern: pattern, alias: alias, isGlob: isGlob, isAll: isAll})
	}
	return s, nil
}

// Match reports whether a container with the given resolved name and id
// is selected, and the alias to display it as when an aliased exact entry
// matched (empty otherwise). Exact entries match the resolved name, the
// full 64-char id, or the 12-char short id.
func (s *Selector) Match(name, id string) (alias string, ok bool) {
	if len(s.entries) == 0 {
		return "", true // no entries: select everything
	}
	short := shortID(id)
	for _, e := range s.entries {
		switch {
		case e.isAll:
			ok = true
		case e.isGlob:
			if m, _ := filepath.Match(e.pattern, name); m {
				ok = true
			}
		default:
			if e.pattern == name || e.pattern == id || e.pattern == short {
				// An exact entry's alias wins over any other match.
				return e.alias, true
			}
		}
	}
	return "", ok
}
