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

	// WALAttestationWriteFailures counts failures to durably record a WAL segment
	// attestation sidecar. Non-fatal: the segment itself is valid, but its contents
	// become unverifiable against whole-record loss.
	WALAttestationWriteFailures = NewCounter()

	// WALAttestationMismatches counts segments whose on-disk contents diverged from
	// their attestation during recovery, indicating whole-record loss that per-record
	// CRC32 cannot detect.
	WALAttestationMismatches = NewCounter()

	// WALAttestationAbsent counts segments with no attestation sidecar, i.e.
	// pre-attestation databases or segments never sealed. These fall back to the
	// unverifiable posture rather than failing recovery.
	WALAttestationAbsent = NewCounter()

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

	// InFlightRequests tracks currently active in-flight requests being processed across all connections.
	InFlightRequests = NewGauge()

	// PipelineLimitHits tracks the number of times a connection hits its maximum in-flight pipeline limit.
	PipelineLimitHits = NewCounter()

	// PipelineRejections tracks requests rejected due to global pipeline capacity saturation.
	PipelineRejections = NewCounter()
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
	DefaultRegistry.RegisterCounter(
		"lattice_wal_attestation_write_failures_total",
		"Total failures to durably record a WAL segment attestation sidecar",
		WALAttestationWriteFailures,
	)
	DefaultRegistry.RegisterCounter(
		"lattice_wal_attestation_mismatches_total",
		"Total WAL segments whose contents diverged from their attestation during recovery",
		WALAttestationMismatches,
	)
	DefaultRegistry.RegisterCounter(
		"lattice_wal_attestation_absent_total",
		"Total WAL segments discovered with no attestation sidecar (unverifiable posture)",
		WALAttestationAbsent,
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
	DefaultRegistry.RegisterGauge(
		"lattice_requests_in_flight",
		"Current number of in-flight requests being processed across all connections",
		InFlightRequests,
	)
	DefaultRegistry.RegisterCounter(
		"lattice_pipeline_limit_hits_total",
		"Total occurrences of a connection reaching its maximum in-flight pipeline limit",
		PipelineLimitHits,
	)
	DefaultRegistry.RegisterCounter(
		"lattice_pipeline_rejections_total",
		"Total requests rejected due to global pipeline saturation",
		PipelineRejections,
	)
}
