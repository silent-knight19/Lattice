package metrics

var (
	// DefaultRegistry is the default singleton metrics registry for the Lattice database process.
	DefaultRegistry = NewRegistry()

	// EngineWriteLatency tracks internal storage engine write execution duration (WAL sync + MemTable insert).
	EngineWriteLatency = NewHistogramVec(
		DefaultLatencyBuckets,
		[]string{"op"},
		map[string][]string{
			"op": {"put", "delete"},
		},
	)

	// EngineReadLatency tracks internal storage engine read execution duration (MemTable + SSTable/Cache).
	EngineReadLatency = NewHistogram(DefaultLatencyBuckets)

	// RaftProposalLatency tracks Raft consensus write duration from proposal to quorum replication commit.
	RaftProposalLatency = NewHistogramVec(
		DefaultLatencyBuckets,
		[]string{"op"},
		map[string][]string{
			"op": {"put", "delete"},
		},
	)

	// RaftReadIndexLatency tracks Raft linearizable ReadIndex quorum heartbeat verification duration.
	RaftReadIndexLatency = NewHistogram(DefaultLatencyBuckets)

	// RequestDuration tracks client-observed end-to-end request duration on the transport wire protocol.
	RequestDuration = NewHistogramVec(
		DefaultLatencyBuckets,
		[]string{"op", "status"},
		map[string][]string{
			"op":     {"put", "get", "delete"},
			"status": {"ok", "error", "not_found", "not_leader", "throttled"},
		},
	)

	// WALBytesWritten counts cumulative bytes appended and synced to WAL log files on disk.
	WALBytesWritten = NewCounter()

	// CompactionDuration tracks leveled compaction execution duration.
	CompactionDuration = NewHistogramVec(
		DefaultCompactionBuckets,
		[]string{"target_level"},
		map[string][]string{
			"target_level": {"1", "2", "3", "4", "5", "6"},
		},
	)

	// FlushDuration tracks duration of background MemTable flushes to Level 0 SSTables.
	FlushDuration = NewHistogram(DefaultLatencyBuckets)

	// BlockCacheHits counts total read block cache hits.
	BlockCacheHits = NewCounter()

	// BlockCacheMisses counts total read block cache misses.
	BlockCacheMisses = NewCounter()

	// ActiveConnections tracks currently open client TCP connections.
	ActiveConnections = NewGauge()
)

func init() {
	DefaultRegistry.RegisterHistogramVec(
		"lattice_engine_write_latency_seconds",
		"Internal storage engine write latency in seconds (WAL append/sync + MemTable insertion)",
		EngineWriteLatency,
	)
	DefaultRegistry.RegisterHistogram(
		"lattice_engine_read_latency_seconds",
		"Internal storage engine read latency in seconds (MemTable lookup + SSTable seek)",
		EngineReadLatency,
	)
	DefaultRegistry.RegisterHistogramVec(
		"lattice_raft_proposal_latency_seconds",
		"Raft consensus proposal latency from client submit to quorum commit",
		RaftProposalLatency,
	)
	DefaultRegistry.RegisterHistogram(
		"lattice_raft_read_index_latency_seconds",
		"Raft linearizable ReadIndex quorum heartbeat verification latency in seconds",
		RaftReadIndexLatency,
	)
	DefaultRegistry.RegisterHistogramVec(
		"lattice_request_duration_seconds",
		"Client request execution duration on binary transport protocol in seconds",
		RequestDuration,
	)
	DefaultRegistry.RegisterCounter(
		"lattice_wal_bytes_written_total",
		"Total cumulative bytes appended and durably synced to Write-Ahead Log segment files",
		WALBytesWritten,
	)
	DefaultRegistry.RegisterHistogramVec(
		"lattice_compaction_duration_seconds",
		"Leveled compaction duration in seconds partitioned by target LSM level",
		CompactionDuration,
	)
	DefaultRegistry.RegisterHistogram(
		"lattice_flush_duration_seconds",
		"Background MemTable flush duration in seconds generating Level 0 SSTables",
		FlushDuration,
	)
	DefaultRegistry.RegisterCounter(
		"lattice_block_cache_hits_total",
		"Total read block cache hit count",
		BlockCacheHits,
	)
	DefaultRegistry.RegisterCounter(
		"lattice_block_cache_misses_total",
		"Total read block cache miss count",
		BlockCacheMisses,
	)
	DefaultRegistry.RegisterGauge(
		"lattice_connections_active",
		"Current number of active client TCP connections",
		ActiveConnections,
	)
}
