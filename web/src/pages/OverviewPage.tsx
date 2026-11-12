import { useState, useEffect } from 'react';
import type { NodeInfo, HealthStatus, MetricSnapshot, RaftState } from '../api/types';
import { Card } from '../components/common/Card';
import { Badge } from '../components/common/Badge';
import { StatusDot } from '../components/common/StatusDot';
import { Sparkline } from '../components/common/Sparkline';
import { CheckCircleIcon, AlertCircleIcon } from '../components/common/Icons';

interface OverviewPageProps {
  nodeInfo: NodeInfo | null;
  health: HealthStatus | null;
  metrics: MetricSnapshot | null;
  raftState: RaftState | null;
}

interface StreamEvent {
  id: string;
  time: string;
  type: string;
  message: string;
}

function formatUptime(seconds: number): string {
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  return `${h}h ${m}m`;
}

export const OverviewPage: React.FC<OverviewPageProps> = ({
  nodeInfo,
  health,
  metrics,
  raftState,
}) => {
  const [opsHistory, setOpsHistory] = useState<number[]>([]);
  const [latencyHistory, setLatencyHistory] = useState<number[]>([]);
  const [memHistory, setMemHistory] = useState<number[]>([]);
  const [events, setEvents] = useState<StreamEvent[]>([]);
  const [sseConnected, setSseConnected] = useState(false);

  useEffect(() => {
    if (!metrics) return;
    setOpsHistory((prev) => [...prev.slice(-19), metrics.opsPerSec]);
    setLatencyHistory((prev) => [...prev.slice(-19), metrics.writeP99Ms]);
    setMemHistory((prev) => [...prev.slice(-19), metrics.memtableBytes / (1024 * 1024)]);
  }, [metrics]);

  useEffect(() => {
    const es = new EventSource('/api/v1/events');
    es.onopen = () => setSseConnected(true);
    es.onerror = () => setSseConnected(false);
    es.onmessage = (ev) => {
      const now = new Date().toTimeString().split(' ')[0];
      setEvents((prev) => [
        { id: `${Date.now()}-${Math.random()}`, time: now, type: ev.type || 'event', message: ev.data },
        ...prev.slice(0, 49),
      ]);
    };
    return () => {
      es.close();
      setSseConnected(false);
    };
  }, []);

  const statusLabel = health ? health.status.toUpperCase() : 'TELEMETRY UNAVAILABLE';
  const isHealthy = health?.status === 'healthy';

  return (
    <div className="space-y-6">
      {/* 1. Status Banner */}
      <div
        className={`rounded-xl border p-4 flex flex-col md:flex-row items-start md:items-center justify-between gap-4 ${
          isHealthy
            ? 'border-emerald-500/30 bg-emerald-950/20'
            : 'border-neutral-800 bg-neutral-900/60'
        }`}
      >
        <div className="flex items-center gap-3">
          <div
            className={`w-9 h-9 rounded-full border flex items-center justify-center ${
              isHealthy
                ? 'bg-emerald-500/20 border-emerald-500/30 text-emerald-400'
                : 'bg-neutral-800 border-neutral-700 text-neutral-400'
            }`}
          >
            {isHealthy ? <CheckCircleIcon size={20} /> : <AlertCircleIcon size={20} />}
          </div>
          <div>
            <div className="flex items-center gap-2 flex-wrap">
              <span className="text-sm font-bold text-neutral-100">{statusLabel}</span>
              {nodeInfo && (
                <>
                  <span className="text-xs text-neutral-400 font-mono">·</span>
                  <span className="text-xs font-mono text-emerald-400 font-semibold">
                    {nodeInfo.role} node-{nodeInfo.nodeId} (Term {nodeInfo.term})
                  </span>
                </>
              )}
            </div>
            <p className="text-xs text-neutral-400 mt-0.5 font-mono">
              {nodeInfo
                ? `${nodeInfo.storageEngine} · ${nodeInfo.version}`
                : 'Waiting for live /api/v1/health and /api/v1/node responses'}
            </p>
          </div>
        </div>
        {health && (
          <div className="flex items-center gap-2 font-mono text-xs">
            <Badge variant={health.quorumHealthy ? 'success' : 'warning'}>
              {health.quorumHealthy ? 'Quorum Healthy' : 'Quorum Degraded'}
            </Badge>
            <Badge variant={health.storageHealthy ? 'level' : 'danger'}>
              {health.storageHealthy ? 'Storage OK' : 'Storage Fault'}
            </Badge>
          </div>
        )}
      </div>

      {/* 2. 6-Tile Row */}
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-6 gap-4">
        {/* Uptime */}
        <Card className="p-4">
          <div className="text-[11px] font-mono text-neutral-400 font-medium">UPTIME</div>
          <div className="text-xl font-bold text-neutral-100 mt-1 font-mono">
            {nodeInfo ? formatUptime(nodeInfo.uptimeSeconds) : '—'}
          </div>
          <div className="mt-2 text-[10px] text-neutral-500 font-mono">
            {nodeInfo ? `Node #${nodeInfo.nodeId}` : 'Unavailable'}
          </div>
        </Card>

        {/* Throughput */}
        <Card className="p-4">
          <div className="text-[11px] font-mono text-neutral-400 font-medium">OPS / SECOND</div>
          <div className="text-xl font-bold text-neutral-100 mt-1 font-mono">
            {metrics ? metrics.opsPerSec.toLocaleString() : '—'}
          </div>
          <div className="mt-2">
            {opsHistory.length >= 2 ? (
              <Sparkline data={opsHistory} color="#10B981" width={120} height={24} />
            ) : (
              <span className="text-[10px] font-mono text-neutral-500">No samples</span>
            )}
          </div>
        </Card>

        {/* Write p99 Latency */}
        <Card className="p-4">
          <div className="text-[11px] font-mono text-neutral-400 font-medium">WRITE P99 LATENCY</div>
          <div className="text-xl font-bold text-neutral-100 mt-1 font-mono">
            {metrics ? `${metrics.writeP99Ms.toFixed(2)} ms` : '—'}
          </div>
          <div className="mt-2">
            {latencyHistory.length >= 2 ? (
              <Sparkline data={latencyHistory} color="#6366F1" width={120} height={24} />
            ) : (
              <span className="text-[10px] font-mono text-neutral-500">No samples</span>
            )}
          </div>
        </Card>

        {/* LSM Total Size */}
        <Card className="p-4">
          <div className="text-[11px] font-mono text-neutral-400 font-medium">LSM TOTAL SIZE</div>
          <div className="text-xl font-bold text-neutral-100 mt-1 font-mono">
            {metrics ? `${(metrics.lsmTotalSizeBytes / (1024 * 1024)).toFixed(1)} MB` : '—'}
          </div>
          <div className="mt-2 text-[10px] text-neutral-500 font-mono">
            {metrics ? `WAL: ${(metrics.walActiveSegmentBytes / (1024 * 1024)).toFixed(1)} MB` : 'Unavailable'}
          </div>
        </Card>

        {/* MemTable Fill % */}
        <Card className="p-4">
          <div className="text-[11px] font-mono text-neutral-400 font-medium">MEMTABLE FILL</div>
          <div className="text-xl font-bold text-neutral-100 mt-1 font-mono">
            {metrics && metrics.memtableLimitBytes > 0
              ? `${Math.round((metrics.memtableBytes / metrics.memtableLimitBytes) * 100)}%`
              : '—'}
          </div>
          <div className="mt-2">
            {memHistory.length >= 2 ? (
              <Sparkline data={memHistory} color="#EC4899" width={120} height={24} />
            ) : (
              <span className="text-[10px] font-mono text-neutral-500">No samples</span>
            )}
          </div>
        </Card>

        {/* Disk Free */}
        <Card className="p-4">
          <div className="text-[11px] font-mono text-neutral-400 font-medium">DISK FREE</div>
          <div className="text-xl font-bold text-neutral-100 mt-1 font-mono">
            {metrics ? `${(metrics.diskFreeBytes / (1024 * 1024 * 1024)).toFixed(1)} GB` : '—'}
          </div>
          <div className="mt-2 text-[10px] text-neutral-400 flex items-center justify-between font-mono">
            <span>Cache Hit Rate</span>
            <span className="text-emerald-400">
              {metrics ? `${metrics.blockCacheHitRate.toFixed(1)}%` : '—'}
            </span>
          </div>
        </Card>
      </div>

      {/* 3. Cluster Strip & Live Event Stream */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
        <Card
          title="Cluster Consensus State"
          subtitle="Raft node and peer replication status"
          badge={
            raftState ? (
              <Badge variant={raftState.role === 'Leader' ? 'leader' : 'follower'}>
                Node {raftState.nodeId} ({raftState.role})
              </Badge>
            ) : undefined
          }
        >
          {raftState ? (
            <div className="space-y-3 mt-1">
              <div className="p-3 rounded-lg bg-neutral-950/70 border border-neutral-800 flex items-center justify-between">
                <div className="flex items-center gap-3">
                  <StatusDot status="healthy" />
                  <div>
                    <div className="text-xs font-semibold font-mono text-neutral-100">
                      node-{raftState.nodeId} (Local)
                    </div>
                    <div className="text-[11px] text-neutral-400 font-mono">
                      Term {raftState.term}
                    </div>
                  </div>
                </div>
                <div className="text-right font-mono">
                  <Badge variant={raftState.role === 'Leader' ? 'leader' : 'follower'}>
                    {raftState.role}
                  </Badge>
                  <div className="text-[10px] text-neutral-400 mt-1">
                    Commit #{raftState.commitIndex}
                  </div>
                </div>
              </div>

              {(raftState.peers || []).map((peer) => (
                <div
                  key={peer.nodeId}
                  className="p-3 rounded-lg bg-neutral-950/70 border border-neutral-800 flex items-center justify-between"
                >
                  <div className="flex items-center gap-3">
                    <StatusDot status={peer.reachable ? 'healthy' : 'error'} />
                    <div>
                      <div className="text-xs font-semibold font-mono text-neutral-100">
                        node-{peer.nodeId} (Peer)
                      </div>
                      <div className="text-[11px] text-neutral-400 font-mono">{peer.address}</div>
                    </div>
                  </div>
                  <div className="text-right font-mono">
                    <Badge variant={peer.role === 'Leader' ? 'leader' : 'follower'}>
                      {peer.role}
                    </Badge>
                    <div className="text-[10px] text-neutral-400 mt-1">
                      Lag: {peer.lagEntries} entries ({peer.lastHeartbeatMs}ms)
                    </div>
                  </div>
                </div>
              ))}
            </div>
          ) : (
            <div className="p-6 text-center text-xs font-mono text-neutral-500">
              No cluster telemetry available from /api/v1/raft/status.
            </div>
          )}
        </Card>

        {/* Live Engine Event Stream */}
        <Card
          title="Recent Storage & Consensus Events"
          subtitle="Event stream from /api/v1/events"
          badge={
            <span
              className={`text-[10px] font-mono flex items-center gap-1.5 ${
                sseConnected ? 'text-emerald-400' : 'text-neutral-500'
              }`}
            >
              <span
                className={`w-1.5 h-1.5 rounded-full ${
                  sseConnected ? 'bg-emerald-500' : 'bg-neutral-600'
                }`}
              />
              {sseConnected ? 'CONNECTED' : 'DISCONNECTED'}
            </span>
          }
        >
          {(events || []).length > 0 ? (
            <div className="space-y-2 mt-1 font-mono text-xs max-h-[220px] overflow-y-auto pr-1">
              {(events || []).map((ev) => (
                <div
                  key={ev.id}
                  className="p-2.5 rounded bg-neutral-950/60 border border-neutral-800/80 flex items-start gap-2.5"
                >
                  <span className="text-emerald-400 font-bold">{ev.time}</span>
                  <div>
                    <span className="text-neutral-200 font-medium">[{ev.type}]</span>
                    <span className="text-neutral-400 ml-1.5">{ev.message}</span>
                  </div>
                </div>
              ))}
            </div>
          ) : (
            <div className="p-6 text-center text-xs font-mono text-neutral-500">
              {sseConnected
                ? 'Connected to /api/v1/events. Waiting for events...'
                : 'Event stream not connected.'}
            </div>
          )}
        </Card>
      </div>
    </div>
  );
};
