package engine_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/wal"
)

func benchmarkRecoverWAL(b *testing.B, count int) {
	dir := b.TempDir()

	records := make([]wal.Record, count)
	for i := 0; i < count; i++ {
		seq := uint64(i + 1)
		records[i] = wal.Record{
			Type:      wal.RecordTypePut,
			SeqNum:    binary.SeqNum(seq),
			Timestamp: 1700000000000000 + seq,
			Key:       []byte(fmt.Sprintf("bench_key_%06d", seq)),
			Value:     []byte(fmt.Sprintf("bench_val_%06d", seq)),
		}
	}
	if _, err := wal.InitDir(dir); err != nil {
		b.Fatalf("failed to init wal dir: %v", err)
	}
	w, err := wal.CreateSegmentWriter(dir, 1)
	if err != nil {
		b.Fatalf("failed to create segment: %v", err)
	}
	for _, rec := range records {
		if err := w.AppendSync(rec); err != nil {
			_ = w.Close()
			b.Fatalf("failed to append: %v", err)
		}
	}
	_ = w.Close()

	cfg := engine.BackpressureConfig{
		MaxMemoryBytes: 128 * 1024 * 1024,
		HighWatermark:  0.80,
		HardWatermark:  0.90,
		MaxWaitTimeout: 100 * time.Millisecond,
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		eng := engine.NewEngineWithOptions(engine.EngineOptions{
			DBPath:       dir,
			Backpressure: cfg,
		})
		if err := eng.RecoverWAL(); err != nil {
			b.Fatalf("RecoverWAL failed: %v", err)
		}
		_ = eng.Close()
	}
}

func BenchmarkEngineRecoverWAL_100Records(b *testing.B) {
	benchmarkRecoverWAL(b, 100)
}

func BenchmarkEngineRecoverWAL_500Records(b *testing.B) {
	benchmarkRecoverWAL(b, 500)
}

func BenchmarkEngineRecoverWAL_1000Records(b *testing.B) {
	benchmarkRecoverWAL(b, 1000)
}
