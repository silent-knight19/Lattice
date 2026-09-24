package engine

import (
	"github.com/silent-knight19/lattice/internal/transport"
	"github.com/silent-knight19/lattice/internal/version"
)

// Stats captures a coherent point-in-time diagnostic snapshot of Engine state.
// Acquires e.mu.RLock briefly to read in-memory metadata without blocking concurrent
// writes or reads, and never performs expensive disk directory scans.
func (e *Engine) Stats() (transport.EngineStats, transport.MemoryStats, transport.StorageStats, transport.CacheStats, error) {
	if e == nil {
		return transport.EngineStats{}, transport.MemoryStats{}, transport.StorageStats{}, transport.CacheStats{}, nil
	}

	e.mu.RLock()
	var stateStr string
	switch e.state {
	case engineStateRecovering:
		stateStr = "recovering"
	case engineStateClosing:
		stateStr = "closing"
	case engineStateClosed:
		stateStr = "closed"
	default:
		stateStr = "open"
	}

	activeEntries := 0
	activeBytes := uint64(0)
	if e.activeMem != nil {
		activeEntries = e.activeMem.Len()
		activeBytes = e.activeMem.ByteSize()
	}

	immCount := len(e.immMems)
	immBytes := uint64(0)
	for _, imm := range e.immMems {
		if imm != nil {
			immBytes += imm.ByteSize()
		}
	}

	l0Files := 0
	totalFiles := 0
	totalBytes := uint64(0)
	if e.vset != nil && e.vset.HasCurrent() {
		ver := e.vset.Current()
		if ver != nil {
			defer ver.Unref()
			l0Files = len(ver.Files(0))
			for lvl := 0; lvl < version.NumLevels; lvl++ {
				files := ver.Files(lvl)
				totalFiles += len(files)
				for _, f := range files {
					totalBytes += f.FileSize
				}
			}
		}
	}

	var bpStats BackpressureStats
	if e.backpressure != nil {
		bpStats = e.backpressure.Stats()
	}

	cacheCap := 0
	cacheEntries := 0
	cacheHits := uint64(0)
	cacheMisses := uint64(0)
	if e.blockCache != nil {
		cacheCap = e.blockCache.Capacity()
		cacheEntries = e.blockCache.Len()
		cacheHits = e.blockCache.Hits()
		cacheMisses = e.blockCache.Misses()
	}
	e.mu.RUnlock()

	hitRatio := 0.0
	if cacheHits+cacheMisses > 0 {
		hitRatio = float64(cacheHits) / float64(cacheHits+cacheMisses)
	}

	engStats := transport.EngineStats{
		State:          stateStr,
		SequenceNumber: e.nextSeqNum.Load(),
		NextFileNumber: e.nextFileNum.Load(),
	}

	memStats := transport.MemoryStats{
		ActiveMemTableEntries:    activeEntries,
		ActiveMemTableBytes:      activeBytes,
		ImmutableMemTableCount:   immCount,
		ImmutableMemTableBytes:   immBytes,
		BackpressureCurrentBytes: bpStats.CurrentMemoryBytes,
		BackpressureMaxBytes:     bpStats.MaxMemoryBytes,
		BackpressureUtilization:  bpStats.Utilization,
		BackpressureThrottled:    bpStats.ThrottledWrites,
		BackpressureRejected:     bpStats.RejectedWrites,
	}

	var activeWALBytes uint64
	var totalWALBytes uint64
	type activeLenReporter interface {
		ActiveLen() int64
	}
	type totalBytesReporter interface {
		TotalBytesWritten() uint64
	}
	if al, ok := e.wal.(activeLenReporter); ok {
		activeWALBytes = uint64(al.ActiveLen())
	}
	if tb, ok := e.wal.(totalBytesReporter); ok {
		totalWALBytes = tb.TotalBytesWritten()
	} else {
		totalWALBytes = activeWALBytes
	}

	storStats := transport.StorageStats{
		L0Files:               l0Files,
		TotalSSTableFiles:     totalFiles,
		TotalSSTableBytes:     totalBytes,
		FlushesCompleted:      e.flushCount.Load(),
		FlushesPending:        immCount,
		ActiveWALSegmentBytes: activeWALBytes,
		WALBytesWritten:       totalWALBytes,
	}

	cStats := transport.CacheStats{
		Capacity: cacheCap,
		Entries:  cacheEntries,
		Hits:     cacheHits,
		Misses:   cacheMisses,
		HitRatio: hitRatio,
	}

	return engStats, memStats, storStats, cStats, nil
}
