package ingestproto_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shellsecrets/peekastokk/internal/ingestproto"
)

func TestLineRoundTrip(t *testing.T) {
	ts := time.Date(2026, 7, 19, 6, 0, 0, 123456789, time.UTC)
	cases := map[string]ingestproto.Line{
		"typical":              {Seq: 42, Source: "nginx", Text: "GET / 200", Off: 1024, Time: ts},
		"unicode":              {Seq: 1, Source: "wörker", Text: "höllo → 世界 🚀", Time: ts},
		"empty text, zero off": {Seq: 2, Source: "app.log", Text: "", Time: ts},
		"newline and quotes in text": {
			Seq: 3, Source: "app", Text: "line with \"quotes\" and \nnewline", Time: ts,
		},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			// NDJSON framing requires one line per message: encoded output
			// must never contain a raw newline.
			if strings.ContainsAny(string(data), "\n") {
				t.Fatalf("encoded line contains a raw newline: %s", data)
			}
			var out ingestproto.Line
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatal(err)
			}
			if out.Seq != in.Seq || out.Source != in.Source || out.Text != in.Text ||
				out.Off != in.Off || !out.Time.Equal(in.Time) {
				t.Fatalf("round trip mismatch:\n in  %+v\n out %+v", in, out)
			}
		})
	}
}

func TestAckRoundTrip(t *testing.T) {
	in := ingestproto.Ack{Ack: 987654321, Time: time.Now().UTC()}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ingestproto.Ack
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Ack != in.Ack || !out.Time.Equal(in.Time) {
		t.Fatalf("round trip mismatch: in %+v out %+v", in, out)
	}
}

// TestWireFieldNames locks in the JSON keys: they are the protocol
// contract between client and server versions, and renaming a struct
// field must not silently change the wire format.
func TestWireFieldNames(t *testing.T) {
	data, _ := json.Marshal(ingestproto.Line{Seq: 1, Source: "s", Text: "t", Off: 2, Time: time.Now()})
	for _, key := range []string{`"seq"`, `"source"`, `"text"`, `"off"`, `"time"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("Line wire format missing %s: %s", key, data)
		}
	}
	ack, _ := json.Marshal(ingestproto.Ack{Ack: 1, Time: time.Now()})
	if !strings.Contains(string(ack), `"ack"`) {
		t.Errorf("Ack wire format missing \"ack\": %s", ack)
	}
}

// TestOffOmittedWhenZero: off is best-effort metadata; zero values stay
// off the wire entirely to keep frames small.
func TestOffOmittedWhenZero(t *testing.T) {
	data, _ := json.Marshal(ingestproto.Line{Seq: 1, Source: "s", Text: "t", Time: time.Now()})
	if strings.Contains(string(data), `"off"`) {
		t.Fatalf("zero Off should be omitted: %s", data)
	}
}
