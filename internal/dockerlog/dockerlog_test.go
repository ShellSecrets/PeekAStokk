package dockerlog_test

import (
	"testing"

	"github.com/shellsecrets/peekastokk/internal/dockerlog"
)

func TestUnwrap(t *testing.T) {
	cases := map[string]struct {
		raw      string
		wantText string
		wantOK   bool
	}{
		"stdout envelope": {
			raw:      `{"log":"hello world\n","stream":"stdout","time":"2026-07-18T11:38:07.475856969Z"}`,
			wantText: "hello world",
			wantOK:   true,
		},
		"stderr envelope": {
			raw:      `{"log":"oops\n","stream":"stderr","time":"2026-07-18T11:38:07.475856969Z"}`,
			wantText: "oops",
			wantOK:   true,
		},
		"no trailing newline (split long line)": {
			raw:      `{"log":"LLLL","stream":"stdout","time":"2026-07-18T11:38:07.483573174Z"}`,
			wantText: "LLLL",
			wantOK:   true,
		},
		"log present but empty": {
			raw:      `{"log":"","stream":"stdout","time":"2026-07-18T11:38:07Z"}`,
			wantText: "",
			wantOK:   true,
		},
		"extra attrs field still unwraps": {
			raw:      `{"log":"tagged\n","stream":"stdout","attrs":{"env":"prod"},"time":"2026-07-18T11:38:07Z"}`,
			wantText: "tagged",
			wantOK:   true,
		},
		"missing log key": {
			raw:    `{"stream":"stdout","time":"2026-07-18T11:38:07Z"}`,
			wantOK: false,
		},
		"wrong stream value": {
			raw:    `{"log":"x\n","stream":"custom","time":"2026-07-18T11:38:07Z"}`,
			wantOK: false,
		},
		"unparseable time": {
			raw:    `{"log":"x\n","stream":"stdout","time":"yesterday"}`,
			wantOK: false,
		},
		"truncated json": {
			raw:    `{"log":"cut off mid`,
			wantOK: false,
		},
		"unrelated json app log": {
			raw:    `{"level":"info","msg":"listening","time":"2026-07-18T11:38:07Z"}`,
			wantOK: false,
		},
		"plain text line": {
			raw:    `GET /index 200 12ms`,
			wantOK: false,
		},
		"empty line": {
			raw:    ``,
			wantOK: false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			text, ok := dockerlog.Unwrap([]byte(tc.raw))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && string(text) != tc.wantText {
				t.Fatalf("text = %q, want %q", text, tc.wantText)
			}
			if !ok && string(text) != tc.raw {
				t.Fatalf("non-match must return raw unchanged, got %q", text)
			}
		})
	}
}
