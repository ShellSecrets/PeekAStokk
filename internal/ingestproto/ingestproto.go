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

// Ack is one server→client status line.
type Ack struct {
	// Ack is the highest client Seq handed to the hub so far on this
	// connection.
	Ack  uint64    `json:"ack"`
	Time time.Time `json:"time"`
}
