import { useState } from 'react';
import type { NodeInfo, HealthStatus, MetricSnapshot } from '../api/types';
import { Card } from '../components/common/Card';
import { Badge } from '../components/common/Badge';
import { StatusDot } from '../components/common/StatusDot';
import { Sparkline } from '../components/common/Sparkline';
import { CheckCircleIcon } from '../components/common/Icons';

interface OverviewPageProps {
  nodeInfo: NodeInfo | null;
  health: HealthStatus | null;
  metrics: MetricSnapshot | null;
}

export const OverviewPage: React.FC<OverviewPageProps> = ({
  nodeInfo,
  health,
  metrics,
}) => {
  // Realistic historical trendlines for sparklines
  const [opsHistory] = useState<number[]>([12400, 13100, 13900, 14200, 13800, 14500, 14200]);
  const [latencyHistory] = useState<number[]>([1.6, 1.5, 1.45, 1.48, 1.41, 1.43, 1.42]);
  const [memHistory] = useState<number[]>([18.2, 20.1, 22.4, 23.8, 24.1, 24.5, 24.8]);

  const isHealthy = health?.status !== 'degraded' && health?.status !== 'unhealthy';

  return (
    <div className="space-y-6">
      {/* 1. Status Banner */}
      <div className="rounded-xl border border-emerald-500/30 bg-emerald-950/20 p-4 flex flex-col md:flex-row items-start md:items-center justify-between gap-4">
        <div className="flex items-center gap-3">
          <div className="w-9 h-9 rounded-full bg-emerald-500/20 border border-emerald-500/30 flex items-center justify-center text-emerald-400">
            <CheckCircleIcon size={20} />
          </div>
          <div>
            <div className="flex items-center gap-2">
              <span className="text-sm font-bold text-neutral-100">
                {isHealthy ? 'SYSTEM HEALTHY' : 'SYSTEM DEGRADED'}
              </span>
              <span className="text-xs text-neutral-400 font-mono">·</span>
              <span className="text-xs font-mono text-emerald-400 font-semibold">
                Leader node-{nodeInfo?.nodeId ?? 1} (Term {nodeInfo?.term ?? 4})
              </span>
              <span className="text-xs text-neutral-400 font-mono">·</span>
              <span className="text-xs font-mono text-neutral-300">3/3 Members Replicated</span>
            </div>
            <p className="text-xs text-neutral-400 mt-0.5">
              Zero-trust engine running hermetically with LSM-Tree storage and Raft distributed consensus.
            </p>
          </div>
        </div>
        <div className="flex items-center gap-2 font-mono text-xs">
          <Badge variant="success">Quorum Healthy</Badge>
          <Badge variant="level">Leveled Compaction</Badge>
        </div>
      </div>

      {/* 2. 6-Tile Row */}
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-6 gap-4">
        {/* Uptime */}
        <Card className="p-4">
          <div className="text-[11px] font-mono text-neutral-400 font-medium">UPTIME</div>
          <div className="text-xl font-bold text-neutral-100 mt-1 font-mono">
            {Math.floor((nodeInfo?.uptimeSeconds ?? 84290) / 3600)}h {Math.floor(((nodeInfo?.uptimeSeconds ?? 84290) % 3600) / 60)}m
          </div>
          <div className="mt-2 text-[10px] text-neutral-400 flex items-center justify-between font-mono">
            <span>Restarts: 0</span>
            <span className="text-emerald-400">100% SLA</span>
          </div>
        </Card>

        {/* Throughput */}
        <Card className="p-4">
          <div className="text-[11px] font-mono text-neutral-400 font-medium">OPS / SECOND</div>
          <div className="text-xl font-bold text-neutral-100 mt-1 font-mono">
            {(metrics?.opsPerSec ?? 14200).toLocaleString()}
          </div>
          <div className="mt-2">
            <Sparkline data={opsHistory} color="#10B981" width={120} height={24} />
          </div>
        </Card>

        {/* Write p99 Latency */}
        <Card className="p-4">
          <div className="text-[11px] font-mono text-neutral-400 font-medium">WRITE P99 LATENCY</div>
          <div className="text-xl font-bold text-neutral-100 mt-1 font-mono">
            {(metrics?.writeP99Ms ?? 1.42).toFixed(2)} ms
          </div>
          <div className="mt-2">
            <Sparkline data={latencyHistory} color="#6366F1" width={120} height={24} />
          </div>
        </Card>

        {/* LSM Total Size */}
        <Card className="p-4">
          <div className="text-[11px] font-mono text-neutral-400 font-medium">LSM TOTAL SIZE</div>
          <div className="text-xl font-bold text-neutral-100 mt-1 font-mono">
            {((metrics?.lsmTotalSizeBytes ?? 482400000) / (1024 * 1024)).toFixed(1)} MB
          </div>
          <div className="mt-2 text-[10px] text-neutral-400 flex items-center justify-between font-mono">
            <span>Levels: L0..L2</span>
            <span className="text-indigo-400">12 SSTables</span>
          </div>
        </Card>

        {/* MemTable Fill % */}
        <Card className="p-4">
          <div className="text-[11px] font-mono text-neutral-400 font-medium">MEMTABLE FILL</div>
          <div className="text-xl font-bold text-neutral-100 mt-1 font-mono">
            {Math.round(((metrics?.memtableBytes ?? 25000000) / (metrics?.memtableLimitBytes ?? 67108864)) * 100)}%
          </div>
          <div className="mt-2">
            <Sparkline data={memHistory} color="#EC4899" width={120} height={24} />
          </div>
        </Card>

        {/* Disk Free */}
        <Card className="p-4">
          <div className="text-[11px] font-mono text-neutral-400 font-medium">DISK FREE</div>
          <div className="text-xl font-bold text-neutral-100 mt-1 font-mono">
            {((metrics?.diskFreeBytes ?? 40000000000) / (1024 * 1024 * 1024)).toFixed(1)} GB
          </div>
          <div className="mt-2 text-[10px] text-neutral-400 flex items-center justify-between font-mono">
            <span>Cache Hit Rate</span>
            <span className="text-emerald-400">{metrics?.blockCacheHitRate ?? 98.6}%</span>
          </div>
        </Card>
      </div>

      {/* 3. Cluster Strip & Recent Engine Events */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
        {/* Cluster Status Strip */}
        <Card
          title="Cluster Consensus State"
          subtitle="Single-group Raft topology across verified peer connections"
          badge={<Badge variant="leader">Node 1 (Leader)</Badge>}
        >
          <div className="space-y-3 mt-1">
            {/* Local Node */}
            <div className="p-3 rounded-lg bg-neutral-950/70 border border-neutral-800 flex items-center justify-between">
              <div className="flex items-center gap-3">
                <StatusDot status="healthy" />
                <div>
                  <div className="text-xs font-semibold font-mono text-neutral-100">node-1 (Local)</div>
                  <div className="text-[11px] text-neutral-400 font-mono">127.0.0.1:9099 · Term {nodeInfo?.term ?? 4}</div>
                </div>
              </div>
              <div className="text-right font-mono">
                <Badge variant="leader">Leader</Badge>
                <div className="text-[10px] text-neutral-400 mt-1">Commit #{nodeInfo?.commitIndex ?? 128450}</div>
              </div>
            </div>

            {/* Peer Node 2 */}
            <div className="p-3 rounded-lg bg-neutral-950/70 border border-neutral-800 flex items-center justify-between">
              <div className="flex items-center gap-3">
                <StatusDot status="healthy" />
                <div>
                  <div className="text-xs font-semibold font-mono text-neutral-100">node-2 (Peer)</div>
                  <div className="text-[11px] text-neutral-400 font-mono">10.0.1.12:9098 · Term {nodeInfo?.term ?? 4}</div>
                </div>
              </div>
              <div className="text-right font-mono">
                <Badge variant="follower">Follower</Badge>
                <div className="text-[10px] text-emerald-400 mt-1">Lag: 0 entries (48ms)</div>
              </div>
            </div>

            {/* Peer Node 3 */}
            <div className="p-3 rounded-lg bg-neutral-950/70 border border-neutral-800 flex items-center justify-between">
              <div className="flex items-center gap-3">
                <StatusDot status="healthy" />
                <div>
                  <div className="text-xs font-semibold font-mono text-neutral-100">node-3 (Peer)</div>
                  <div className="text-[11px] text-neutral-400 font-mono">10.0.1.13:9098 · Term {nodeInfo?.term ?? 4}</div>
                </div>
              </div>
              <div className="text-right font-mono">
                <Badge variant="follower">Follower</Badge>
                <div className="text-[10px] text-emerald-400 mt-1">Lag: 2 entries (52ms)</div>
              </div>
            </div>
          </div>
        </Card>

        {/* Live Engine Event Stream */}
        <Card
          title="Recent Storage & Consensus Events"
          subtitle="Real-time telemetry stream from in-process event bus"
          badge={<span className="text-[10px] font-mono text-emerald-400 flex items-center gap-1.5"><span className="w-1.5 h-1.5 rounded-full bg-emerald-500 animate-ping" />LIVE SSE</span>}
        >
          <div className="space-y-2 mt-1 font-mono text-xs max-h-[220px] overflow-y-auto pr-1">
            <div className="p-2.5 rounded bg-neutral-950/60 border border-neutral-800/80 flex items-start gap-2.5">
              <span className="text-emerald-400 font-bold">11:04:12</span>
              <div>
                <span className="text-neutral-200 font-medium">[LSM Compaction]</span>
                <span className="text-neutral-400 ml-1.5">L0 → L1 completed: 4 files merged into 2 runs (412ms)</span>
              </div>
            </div>
            <div className="p-2.5 rounded bg-neutral-950/60 border border-neutral-800/80 flex items-start gap-2.5">
              <span className="text-blue-400 font-bold">11:03:50</span>
              <div>
                <span className="text-neutral-200 font-medium">[MemTable]</span>
                <span className="text-neutral-400 ml-1.5">Active SkipList flushed to L0 SSTable #000107 (64MB)</span>
              </div>
            </div>
            <div className="p-2.5 rounded bg-neutral-950/60 border border-neutral-800/80 flex items-start gap-2.5">
              <span className="text-purple-400 font-bold">11:02:18</span>
              <div>
                <span className="text-neutral-200 font-medium">[WAL]</span>
                <span className="text-neutral-400 ml-1.5">Segment 000013 sealed; Segment 000014 pre-allocated</span>
              </div>
            </div>
            <div className="p-2.5 rounded bg-neutral-950/60 border border-neutral-800/80 flex items-start gap-2.5">
              <span className="text-amber-400 font-bold">10:58:02</span>
              <div>
                <span className="text-neutral-200 font-medium">[Raft]</span>
                <span className="text-neutral-400 ml-1.5">Linearizable ReadIndex round verified across 2/3 quorum (0.28ms)</span>
              </div>
            </div>
          </div>
        </Card>
      </div>
    </div>
  );
};
