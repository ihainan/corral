package convo

import (
	"bytes"
	"os"
	"testing"
)

// BenchmarkClaudeFeedReal parses the last 4 MiB of the log at
// CORRAL_BENCH_LOG, as a conversation is first read. Skipped without it.
func BenchmarkClaudeFeedReal(b *testing.B) {
	name := os.Getenv("CORRAL_BENCH_LOG")
	if name == "" {
		b.Skip("CORRAL_BENCH_LOG not set")
	}
	data, err := os.ReadFile(name)
	if err != nil {
		b.Fatal(err)
	}
	if len(data) > 4<<20 {
		data = data[len(data)-4<<20:]
		data = data[bytes.IndexByte(data, '\n')+1:]
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		NewClaude().Feed(data)
	}
}
