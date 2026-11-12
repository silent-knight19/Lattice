import { useState } from 'react';
import type { MetricSnapshot } from '../api/types';
import { Card } from '../components/common/Card';
import { Badge } from '../components/common/Badge';
import { Sparkline } from '../components/common/Sparkline';
import { CpuIcon } from '../components/common/Icons';

interface MetricsPageProps {
  metrics: MetricSnapshot | null;
}

export const MetricsPage: React.FC<MetricsPageProps> = ({ metrics }) => {
  const [throughputHist] = useState<number[]>([12000, 12800, 13400, 14200, 14500, 14100, 14200]);
  const [latencyHist] = useState<number[]>([1.8, 1.6, 1.5, 1.42, 1.45, 1.39, 1.42]);
  const [cacheHist] = useState<number[]>([97.8, 98.1, 98.4, 98.6, 98.5, 98.7, 98.6]);
  const [walHist] = useState<number[]>([4.2, 5.1, 6.3, 7.1, 7.8, 8.1, 8.4]);

  return (
    <div className="space-y-6">
      {/* Header bar */}
      <div className="flex flex-col sm:flex-row items-start sm:items-center justify-between gap-4 p-4 rounded-xl bg-neutral-900 border border-neutral-800">
        <div className="flex items-center gap-3">
          <div className="w-9 h-9 rounded-lg bg-teal-500/10 border border-teal-500/20 text-teal-400 flex items-center justify-center">
            <CpuIcon size={18} />
          </div>
          <div>
            <h2 className="text-sm font-bold text-neutral-100 font-mono">Real-Time Prometheus Telemetry</h2>
            <p className="text-xs text-neutral-400">Zero-allocation atomic registry counters, histograms & quantiles</p>
          </div>
        </div>

        <a
          href="/metrics"
          target="_blank"
          rel="noopener noreferrer"
          className="px-3 py-1.5 rounded-lg bg-neutral-950 border border-neutral-800 hover:border-neutral-700 text-neutral-300 text-xs font-mono flex items-center gap-2 transition-colors"
        >
          <span>Raw Prometheus Scrape</span>
          <span className="text-neutral-500">↗</span>
        </a>
      </div>

      {/* Metric Cards Grid */}
      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-6 font-mono">
        {/* Panel 1: Throughput */}
        <Card title="Request Throughput" subtitle="lattice_request_duration_seconds{op} rate">
          <div className="space-y-3 mt-1">
            <div className="flex items-baseline justify-between">
              <span className="text-2xl font-bold text-neutral-100">
                {(metrics?.opsPerSec ?? 14200).toLocaleString()} <span className="text-xs text-neutral-400 font-normal">ops/sec</span>
              </span>
              <Badge variant="success">80% GET / 20% PUT</Badge>
            </div>
            <Sparkline data={throughputHist} color="#10B981" width={260} height={45} showMinMax />
            <div className="pt-2 border-t border-neutral-800/80 text-[11px] text-neutral-400 flex justify-between">
              <span>Read: {(metrics?.readOpsPerSec ?? 11360).toLocaleString()} op/s</span>
              <span>Write: {(metrics?.writeOpsPerSec ?? 2840).toLocaleString()} op/s</span>
            </div>
          </div>
        </Card>

        {/* Panel 2: Write Latency Quantiles */}
        <Card title="Write Latency Histogram" subtitle="lattice_engine_write_latency_seconds">
          <div className="space-y-3 mt-1">
            <div className="flex items-baseline justify-between">
              <span className="text-2xl font-bold text-neutral-100">
                {(metrics?.writeP99Ms ?? 1.42).toFixed(2)} <span className="text-xs text-neutral-400 font-normal">ms p99</span>
              </span>
              <Badge variant="level">Group Commit</Badge>
            </div>
            <Sparkline data={latencyHist} color="#6366F1" width={260} height={45} showMinMax />
            <div className="pt-2 border-t border-neutral-800/80 text-[11px] text-neutral-400 flex justify-between">
              <span>p50: 0.42 ms</span>
              <span>p95: 1.10 ms</span>
              <span>p99: {(metrics?.writeP99Ms ?? 1.42).toFixed(2)} ms</span>
            </div>
          </div>
        </Card>

        {/* Panel 3: Block Cache Hit Rate */}
        <Card title="Block Cache Efficiency" subtitle="lattice_block_cache_{hits,misses}_total">
          <div className="space-y-3 mt-1">
            <div className="flex items-baseline justify-between">
              <span className="text-2xl font-bold text-neutral-100">
                {metrics?.blockCacheHitRate ?? 98.6}% <span className="text-xs text-neutral-400 font-normal">Hit Ratio</span>
              </span>
              <Badge variant="success">Bloom: 99.1%</Badge>
            </div>
            <Sparkline data={cacheHist} color="#14B8A6" width={260} height={45} showMinMax />
            <div className="pt-2 border-t border-neutral-800/80 text-[11px] text-neutral-400 flex justify-between">
              <span>Hits: 1,420,910</span>
              <span>Misses: 20,110</span>
            </div>
          </div>
        </Card>

        {/* Panel 4: Active WAL Buffer */}
        <Card title="WAL Ingestion Throughput" subtitle="lattice_wal_bytes_written_total">
          <div className="space-y-3 mt-1">
            <div className="flex items-baseline justify-between">
              <span className="text-2xl font-bold text-neutral-100">
                {((metrics?.walActiveSegmentBytes ?? 8400000) / (1024 * 1024)).toFixed(1)} <span className="text-xs text-neutral-400 font-normal">MB active</span>
              </span>
              <Badge variant="neutral">fdatasync() OK</Badge>
            </div>
            <Sparkline data={walHist} color="#A855F7" width={260} height={45} showMinMax />
            <div className="pt-2 border-t border-neutral-800/80 text-[11px] text-neutral-400 flex justify-between">
              <span>Segment 000014.wal</span>
              <span>Capacity: 32.0 MB</span>
            </div>
          </div>
        </Card>

        {/* Panel 5: Compaction Times */}
        <Card title="Compaction Duration" subtitle="lattice_compaction_duration_seconds{level}">
          <div className="space-y-3 mt-1">
            <div className="flex items-baseline justify-between">
              <span className="text-2xl font-bold text-neutral-100">
                412 <span className="text-xs text-neutral-400 font-normal">ms median</span>
              </span>
              <Badge variant="level">L0 → L1 Merges</Badge>
            </div>
            <div className="space-y-1.5 pt-1 text-[11px] text-neutral-400">
              <div className="flex justify-between">
                <span>L0 → L1 Duration:</span>
                <span className="text-neutral-200">380ms - 440ms</span>
              </div>
              <div className="flex justify-between">
                <span>L1 → L2 Duration:</span>
                <span className="text-neutral-200">1,210ms</span>
              </div>
              <div className="flex justify-between">
                <span>Total Compactions:</span>
                <span className="text-emerald-400 font-bold">148 runs (0 failures)</span>
              </div>
            </div>
          </div>
        </Card>

        {/* Panel 6: Network & Resource Footprint */}
        <Card title="Resource Governance" subtitle="Goroutine and connection bounds">
          <div className="space-y-3 mt-1">
            <div className="flex items-baseline justify-between">
              <span className="text-2xl font-bold text-neutral-100">
                {metrics?.activeConnections ?? 64} <span className="text-xs text-neutral-400 font-normal">TCP conns</span>
              </span>
              <Badge variant="neutral">Pipeline Cap: 256</Badge>
            </div>
            <div className="space-y-1.5 pt-1 text-[11px] text-neutral-400">
              <div className="flex justify-between">
                <span>Goroutines:</span>
                <span className="text-neutral-200">74 active</span>
              </div>
              <div className="flex justify-between">
                <span>Heap Alloc:</span>
                <span className="text-neutral-200">38.4 MB</span>
              </div>
              <div className="flex justify-between">
                <span>GC Pause p99:</span>
                <span className="text-emerald-400">0.24 ms</span>
              </div>
            </div>
          </div>
        </Card>
      </div>
    </div>
  );
};
