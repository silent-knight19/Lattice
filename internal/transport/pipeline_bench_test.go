package transport_test

import (
	"context"
	"net"
	"testing"

	"github.com/silent-knight19/lattice/internal/transport"
	"github.com/silent-knight19/lattice/pkg/client"
)

// Note on Benchmark Methodology and Scope:
// These benchmarks measure transport framing overhead, TCP wire multiplexing, and
// concurrent dispatch efficiency over loopback. They intentionally use an in-memory
// mockEngine (zero disk I/O, zero WAL fsync, zero compaction overhead) to isolate
// transport-layer performance from storage-engine disk latency.
//
// The observed throughput speedups (e.g. 1.4x at depth 2, 3.2x at depth 8, 3.4x at depth 32)
// represent protocol-level and framing pipelining gains over lockstep request-response RTTs,
// and must NOT be interpreted as generic production database write throughput where disk I/O
// and WAL durability dominate.

// BenchmarkSequential measures baseline lockstep request-response throughput over TCP.
func BenchmarkSequential(b *testing.B) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	srv := startTestServer(&testing.T{}, cfg, eng)
	defer srv.Close()

	c, err := client.Dial(srv.Addr().String())
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	key := []byte("bench_key")
	val := []byte("bench_value")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := c.Put(ctx, key, val); err != nil {
			b.Fatalf("put failed: %v", err)
		}
	}
}

func benchmarkPipelineDepth(b *testing.B, depth int) {
	eng := newMockEngine()
	cfg := transport.DefaultServerConfig()
	srv := startTestServer(&testing.T{}, cfg, eng)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	key := []byte("bench_pipe_key")
	val := []byte("bench_pipe_val")

	reqs := make([]*transport.Request, depth)
	for i := 0; i < depth; i++ {
		reqs[i] = &transport.Request{
			OpCode: transport.OpPut,
			Key:    key,
			Value:  val,
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	var seq uint64
	for i := 0; i < b.N; i++ {
		for d := 0; d < depth; d++ {
			seq++
			reqs[d].SeqID = seq
			if err := transport.WriteRequest(conn, reqs[d]); err != nil {
				b.Fatalf("write req: %v", err)
			}
		}
		for d := 0; d < depth; d++ {
			resp, err := transport.ReadResponse(conn)
			if err != nil || resp.Status != transport.StatusOk {
				b.Fatalf("read resp: %v, status: %v", err, resp.Status)
			}
		}
	}
	b.SetBytes(int64(depth * (len(key) + len(val))))
}

func BenchmarkPipeline_Depth2(b *testing.B) {
	benchmarkPipelineDepth(b, 2)
}

func BenchmarkPipeline_Depth8(b *testing.B) {
	benchmarkPipelineDepth(b, 8)
}

func BenchmarkPipeline_Depth32(b *testing.B) {
	benchmarkPipelineDepth(b, 32)
}
