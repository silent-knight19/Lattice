package transport

// EngineStats captures high-level storage engine lifecycle and sequence metadata.
type EngineStats struct {
	State          string `json:"state"`
	SequenceNumber uint64 `json:"sequence_number"`
	NextFileNumber uint64 `json:"next_file_number"`
}

// MemoryStats captures in-memory MemTable occupancy and backpressure controller telemetry.
type MemoryStats struct {
	ActiveMemTableEntries    int     `json:"active_memtable_entries"`
	ActiveMemTableBytes      uint64  `json:"active_memtable_bytes"`
	ImmutableMemTableCount   int     `json:"immutable_memtable_count"`
	ImmutableMemTableBytes   uint64  `json:"immutable_memtable_bytes"`
	BackpressureCurrentBytes uint64  `json:"backpressure_current_bytes"`
	BackpressureMaxBytes     uint64  `json:"backpressure_max_bytes"`
	BackpressureUtilization  float64 `json:"backpressure_utilization"`
	BackpressureThrottled    uint64  `json:"backpressure_throttled_writes"`
	BackpressureRejected     uint64  `json:"backpressure_rejected_writes"`
}

// StorageStats captures on-disk SSTable and WAL storage telemetry.
type StorageStats struct {
	L0Files           int    `json:"l0_files"`
	TotalSSTableFiles int    `json:"total_sstable_files"`
	TotalSSTableBytes uint64 `json:"total_sstable_bytes"`
	FlushesCompleted  uint64 `json:"flushes_completed"`
	FlushesPending    int    `json:"flushes_pending"`
	WALBytesWritten   uint64 `json:"wal_bytes_written"`
}

// CacheStats captures block cache capacity, occupancy, and hit/miss telemetry.
type CacheStats struct {
	Capacity int     `json:"capacity"`
	Entries  int     `json:"entries"`
	Hits     uint64  `json:"hits"`
	Misses   uint64  `json:"misses"`
	HitRatio float64 `json:"hit_ratio"`
}

// ClusterStats captures Raft consensus and node identity telemetry.
type ClusterStats struct {
	Enabled     bool   `json:"enabled"`
	Role        string `json:"role,omitempty"`
	Term        uint64 `json:"term,omitempty"`
	LocalID     uint64 `json:"local_id,omitempty"`
	LeaderID    uint64 `json:"leader_id,omitempty"`
	CommitIndex uint64 `json:"commit_index,omitempty"`
	LastApplied uint64 `json:"last_applied,omitempty"`
}

// ConnStats captures client connection counts.
type ConnStats struct {
	Active int64 `json:"active"`
}

// StatsSnapshot represents the complete, structured point-in-time diagnostic snapshot.
type StatsSnapshot struct {
	Engine      EngineStats  `json:"engine"`
	Memory      MemoryStats  `json:"memory"`
	Storage     StorageStats `json:"storage"`
	Cache       CacheStats   `json:"cache"`
	Cluster     ClusterStats `json:"cluster"`
	Connections ConnStats    `json:"connections"`
}

// ClusterStatsProvider allows cluster routers to expose consensus node metadata to Stats.
type ClusterStatsProvider interface {
	ClusterStats() ClusterStats
}
