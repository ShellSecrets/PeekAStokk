// Package dockerlog detects and unwraps Docker's json-file log driver
// envelope, so container logs display as plain text instead of raw JSON,
// without any opt-in configuration.
package dockerlog

import (
	"bytes"
	"encoding/json"
	"time"
)

// envelope mirrors one line of Docker's json-file log driver output, e.g.:
//
//	{"log":"actual output\n","stream":"stdout","time":"2023-01-01T00:00:00.000000000Z"}
//
// Fields beyond these three (e.g. "attrs", present when the container was
// started with --log-opt labels/env) are intentionally not modeled:
// encoding/json ignores unrecognized keys by default, so envelopes with
// extra fields still match.
type envelope struct {
	Log    *string `json:"log"`
	Stream string  `json:"stream"`
	Time   string  `json:"time"`
}

// Unwrap reports whether raw is one line of a Docker json-file log
// envelope and, if so, returns the inner "log" value with its trailing
// newline trimmed (Docker embeds the captured line's own newline inside
// the JSON string). ok is false for anything not confidently this shape —
// truncated/partial JSON, unrelated JSON-emitting apps — in which case
// callers must display raw unchanged. Never errors and never panics.
//
// Detection requires the "log" field to be present (even if empty),
// "stream" to be exactly "stdout" or "stderr" (the only values Docker
// ever writes), and "time" to parse as Docker's RFC3339Nano timestamp.
// A false positive would need an unrelated app to emit all three together
// and costs only cosmetics: raw bytes on disk and byte offsets are never
// affected by unwrapping.
func Unwrap(raw []byte) (text []byte, ok bool) {
	// Cheap reject: most log lines are not JSON at all.
	if len(raw) < 2 || raw[0] != '{' {
		return raw, false
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return raw, false
	}
	if env.Log == nil {
		return raw, false // "log" key absent: not this shape
	}
	if env.Stream != "stdout" && env.Stream != "stderr" {
		return raw, false
	}
	if _, err := time.Parse(time.RFC3339Nano, env.Time); err != nil {
		return raw, false
	}
	return bytes.TrimSuffix([]byte(*env.Log), []byte("\n")), true
}
