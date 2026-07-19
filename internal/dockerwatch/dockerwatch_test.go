package dockerwatch_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shellsecrets/peekastokk/internal/dockerwatch"
)

// fakeContainer creates <root>/<id>/ with a json log file and, unless
// cfg is empty, a config.v2.json with the given content.
func fakeContainer(t *testing.T, root, id, cfg string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+"-json.log"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.v2.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	idNginx  = "0216a71e0394ea2113c91d67e763534550a2180e5e25e7ad3e2bd3838f9f25e6"
	idWorker = "1327b82f1405fb3224da2e78f874645661b3291f6f36f8be4f3ce4949f0f36f7"
	idBroken = "2438c93f2516fc4335eb3f89f985756772c43a2f7f47f9cf5f4df5a5af1f47f8"
)

func newWatcher(t *testing.T, root string, entries []string) *dockerwatch.Watcher {
	t.Helper()
	sel, err := dockerwatch.NewSelector(entries)
	if err != nil {
		t.Fatal(err)
	}
	return dockerwatch.NewWatcher(root, sel, nil)
}

func names(cs []dockerwatch.Container) map[string]string {
	out := make(map[string]string, len(cs))
	for _, c := range cs {
		out[c.ID] = c.DisplayName
	}
	return out
}

func TestScanResolvesNamesAndFallsBack(t *testing.T) {
	root := t.TempDir()
	fakeContainer(t, root, idNginx, `{"ID":"`+idNginx+`","Name":"/nginx"}`)
	fakeContainer(t, root, idWorker, "") // no config.v2.json at all
	fakeContainer(t, root, idBroken, `{torn json`)

	got := names(newWatcher(t, root, nil).Scan())
	if got[idNginx] != "nginx" {
		t.Errorf("resolved name = %q, want nginx", got[idNginx])
	}
	if got[idWorker] != idWorker[:12] {
		t.Errorf("missing config fallback = %q, want short id", got[idWorker])
	}
	if got[idBroken] != idBroken[:12] {
		t.Errorf("malformed config fallback = %q, want short id", got[idBroken])
	}
}

func TestScanTracksAppearAndDisappear(t *testing.T) {
	root := t.TempDir()
	w := newWatcher(t, root, nil)

	if got := w.Scan(); len(got) != 0 {
		t.Fatalf("empty root scan = %+v", got)
	}
	fakeContainer(t, root, idNginx, `{"Name":"/nginx"}`)
	if got := w.Scan(); len(got) != 1 {
		t.Fatalf("after create scan = %+v", got)
	}
	if err := os.RemoveAll(filepath.Join(root, idNginx)); err != nil {
		t.Fatal(err)
	}
	if got := w.Scan(); len(got) != 0 {
		t.Fatalf("after remove scan = %+v", got)
	}
}

func TestScanMissingRootIsEmptyNotError(t *testing.T) {
	w := newWatcher(t, filepath.Join(t.TempDir(), "not-yet-mounted"), nil)
	if got := w.Scan(); got != nil {
		t.Fatalf("missing root scan = %+v, want nil", got)
	}
}

func TestSelectorForms(t *testing.T) {
	root := t.TempDir()
	fakeContainer(t, root, idNginx, `{"Name":"/nginx-prod"}`)
	fakeContainer(t, root, idWorker, `{"Name":"/worker"}`)

	cases := map[string]struct {
		entries []string
		want    map[string]string // id -> display
	}{
		"all star": {
			entries: []string{"*"},
			want:    map[string]string{idNginx: "nginx-prod", idWorker: "worker"},
		},
		"exact name": {
			entries: []string{"worker"},
			want:    map[string]string{idWorker: "worker"},
		},
		"exact name aliased": {
			entries: []string{"nginx-prod:web"},
			want:    map[string]string{idNginx: "web"},
		},
		"exact short id aliased": {
			entries: []string{idWorker[:12] + ":background"},
			want:    map[string]string{idWorker: "background"},
		},
		"exact full id": {
			entries: []string{idNginx},
			want:    map[string]string{idNginx: "nginx-prod"},
		},
		"glob": {
			entries: []string{"nginx-*"},
			want:    map[string]string{idNginx: "nginx-prod"},
		},
		"glob plus aliased exact": {
			entries: []string{"nginx-*", "worker:jobs"},
			want:    map[string]string{idNginx: "nginx-prod", idWorker: "jobs"},
		},
		"no match": {
			entries: []string{"absent"},
			want:    map[string]string{},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := names(newWatcher(t, root, tc.entries).Scan())
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for id, display := range tc.want {
				if got[id] != display {
					t.Errorf("id %s display = %q, want %q", id[:12], got[id], display)
				}
			}
		})
	}
}

func TestSelectorRejectsAliasOnPatterns(t *testing.T) {
	for _, entries := range [][]string{
		{"*:custom"},
		{"web-*:custom"},
		{":alias"},
		{"name:"},
		{""},
	} {
		if _, err := dockerwatch.NewSelector(entries); err == nil {
			t.Errorf("entries %v: expected error", entries)
		}
	}
}
