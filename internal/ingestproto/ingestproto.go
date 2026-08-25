// Package ingestproto defines the wire format between a forwarding
// PeekAStokk client and a receiving server: newline-delimited JSON over a
// single long-lived HTTP request. The request body carries client→server
// Lines; the response body carries server→client Acks. It is a leaf
// package imported by both internal/server and internal/forward so
// neither depends on the other.
package ingestproto

import "time"

// MaxLineBytes bounds one NDJSON-encoded line on the wire, so a broken or
// hostile peer cannot make the reader allocate without bound.
const MaxLineBytes = 256 * 1024

// Line is one forwarded log line.
type Line struct {
	// Seq is a client-local, monotonically increasing counter for the
	// current connection; the server echoes the highest Seq it has
	// durably published back in Acks, letting the client trim its retry
	// buffer. It is unrelated to the server hub's own sequence numbers.
	Seq uint64 `json:"seq"`
	// Source names the origin on the client — a container's display name
	// or a file's base name. It never carries the client's own identity;
	// that comes solely from the authenticated connection.
	Source string `json:"source"`
	Text   string `json:"text"`
	// Off is the line's byte offset in its source file on the client,
	// best-effort; the server never uses it for disk access.
	Off  int64     `json:"off,omitempty"`
	Time time.Time `json:"time"`
}

// Ack is one server→client message: a delivery acknowledgment, a remote
// scrollback request, or both. Old clients that only understand the ack
// field simply ignore Req (unknown JSON keys are skipped), and a message
// with no ack progress carries Ack 0, which trims nothing.
type Ack struct {
	// Ack is the highest client Seq handed to the hub so far on this
	// connection.
	Ack uint64 `json:"ack,omitempty"`
	// Req asks the client to read older lines from one of its sources'
	// files on disk (the server has no local copy of forwarded logs).
	Req  *BackfillReq `json:"req,omitempty"`
	Time time.Time    `json:"time"`
}

// BackfillReq is a server→client remote scrollback request.
type BackfillReq struct {
	ID     uint64 `json:"id"`     // correlates the response chunks
	Source string `json:"source"` // the client's own name for the source
	Before int64  `json:"before"` // byte offset to read strictly before; <0 = EOF
	Limit  int    `json:"limit"`
}

// BackfillResp is one client→server response chunk for a BackfillReq. A
// response may span several chunks (each stays under MaxLineBytes on the
// wire); Final marks the last one. It travels on the same NDJSON stream
// as Lines, wrapped as {"resp":{...}} — old servers decode that as a Line
// with an empty Source and skip it.
type BackfillResp struct {
	ID      uint64         `json:"id"`
	Lines   []BackfillLine `json:"lines,omitempty"`
	AtStart bool           `json:"at_start,omitempty"`
	Final   bool           `json:"final,omitempty"`
	Err     string         `json:"err,omitempty"` // e.g. source unknown to the client
}

// BackfillLine mirrors backfill.Line without importing it.
type BackfillLine struct {
	Off  int64  `json:"off"`
	Text string `json:"text"`
}

// Up is the server-side decode envelope for one client→server NDJSON
// message: a bare Line (the normal case, fields at top level), a wrapped
// backfill response, or a source announcement.
type Up struct {
	Line
	Resp *BackfillResp `json:"resp,omitempty"`
	// Sources announces every source this client currently tails, sent on
	// each connect (and again when the set changes) so a server that has
	// just restarted knows about quiet files instead of waiting for one
	// of them to produce a line. Old servers decode it as a Line with an
	// empty Source and skip it.
	Sources []string `json:"sources,omitempty"`
}

// Announce is the client→server source announcement, encoded as
// {"sources":[...]}.
type Announce struct {
	Sources []string `json:"sources"`
}
