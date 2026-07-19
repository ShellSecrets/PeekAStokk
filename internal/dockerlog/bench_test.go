package dockerlog

import "testing"

var (
	dockerLine = []byte(`{"log":"GET /api/users 200 12ms request served\n","stream":"stdout","time":"2026-07-18T11:38:07.475856969Z"}`)
	plainLine  = []byte(`2026-07-18 11:38:07 INFO GET /api/users 200 12ms request served`)
)

func BenchmarkUnwrapDockerLine(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		Unwrap(dockerLine)
	}
}

func BenchmarkUnwrapPlainLine(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		Unwrap(plainLine)
	}
}
