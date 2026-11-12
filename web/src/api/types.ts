// TypeScript definitions for Lattice Engine & Console API

export type NodeRole = 'Leader' | 'Follower' | 'Candidate' | 'Standalone';

export interface SystemHostStats {
  os: string;
  arch: string;
  numCPU: number;
  numGoroutine: number;
  heapAllocBytes: number;
  sysMemBytes: number;
  gcRuns: number;
  hostname: string;
  pid: number;
}

export interface NodeInfo {
  nodeId: number;
  role: NodeRole;
  term: number;
  leaderId: number;
  commitIndex: number;
  appliedIndex: number;
  uptimeSeconds: number;
  version: string;
  dataDirBase: string;
  storageEngine: string;
  peersCount: number;
  quorumSize: number;
  system?: SystemHostStats;
}

export interface HealthStatus {
  status: 'healthy' | 'degraded' | 'recovering' | 'unhealthy';
  liveness: boolean;
  readiness: boolean;
  isLeader: boolean;
  quorumHealthy: boolean;
  storageHealthy: boolean;
  walPoisoned: boolean;
  activeIncidents: string[];
}

export interface MetricSnapshot {
  opsPerSec: number;
  readOpsPerSec: number;
  writeOpsPerSec: number;
  writeP99Ms: number;
  readP99Ms: number;
  memtableBytes: number;
  memtableLimitBytes: number;
  blockCacheHitRate: number;
  lsmTotalSizeBytes: number;
  diskFreeBytes: number;
  diskTotalBytes: number;
  activeConnections: number;
  walActiveSegmentBytes: number;
  heapAllocBytes?: number;
  sysMemBytes?: number;
  goroutines?: number;
}

export interface SSTableFile {
  fileNum: number;
  level: number;
  sizeBytes: number;
  entryCount: number;
  smallestKey: string;
  largestKey: string;
  smallestSeqNum: number;
  largestSeqNum: number;
  hasOverlap?: boolean;
}

export interface LSMTreeState {
  levels: {
    level: number;
    files: SSTableFile[];
    totalSizeBytes: number;
    score: number;
  }[];
  memtable: {
    activeBytes: number;
    activeEntries: number;
    immutableCount: number;
    immutableBytes: number;
    flushThresholdBytes: number;
  };
  compaction: {
    running: boolean;
    totalRuns: number;
    failures: number;
    lastCompactionDesc: string;
    lastDurationMs: number;
    writeAmplification: number;
  };
}

export interface RaftPeer {
  nodeId: number;
  address: string;
  role: NodeRole;
  matchIndex: number;
  nextIndex: number;
  lagEntries: number;
  lastHeartbeatMs: number;
  reachable: boolean;
}

export interface RaftState {
  nodeId: number;
  role: NodeRole;
  term: number;
  leaderId: number;
  commitIndex: number;
  lastLogIndex: number;
  lastLogTerm: number;
  quorumSize: number;
  peers: RaftPeer[];
  timeline: {
    term: number;
    leaderId: number;
    startIndex: number;
    endIndex: number;
    durationMs: number;
  }[];
  readIndexLatencyMs: number;
}

export interface KeyEntry {
  key: string;
  value: string;
  valueSizeBytes: number;
  seqNumber: number;
  originLevel: string; // e.g. "MemTable", "L0", "L1"
  isTombstone: boolean;
  timestamp: string;
}

export interface WALSegment {
  segmentNumber: number;
  filename: string;
  sizeBytes: number;
  recordCount: number;
  status: 'active' | 'sealed' | 'checkpointed';
  firstLSN: number;
  lastLSN: number;
  attestationStatus: 'valid' | 'corrupted';
}

export interface WALRecord {
  lsn: number;
  type: 'PUT' | 'DELETE' | 'BATCH' | 'COMMIT' | 'CHECKPOINT';
  key: string;
  valSizeBytes: number;
  crcValid: boolean;
  timestamp: string;
}

export interface BlockInspection {
  index: number;
  offset: number;
  sizeBytes: number;
  crcStored: string;
  crcComputed: string;
  crcValid: boolean;
  recordsCount: number;
  firstKey: string;
  lastKey: string;
}

export interface SSTableInspection {
  filename: string;
  fileSizeBytes: number;
  magicValid: boolean;
  level: number;
  entriesCount: number;
  blocks: BlockInspection[];
  bloomFilter: {
    present: boolean;
    bitsCount: number;
    hashFunctionsCount: number;
    keysCount: number;
    fpRateEstimate: number;
  };
  footer: {
    indexOffset: number;
    indexSize: number;
    metaOffset: number;
    metaSize: number;
    magic: string;
  };
}

export interface SystemConfig {
  nodeId: number;
  address: string;
  adminAddress: string;
  metricsAddress: string;
  dataDir: string;
  maxMemtableBytes: number;
  compactionL0Trigger: number;
  insecureTransport: boolean;
  clientTLSRequired: boolean;
  peerTLSEnabled: boolean;
  authzPolicySummary: string;
}
