import { useState, useEffect } from 'react';
import type { MetricSnapshot } from '../api/types';
import { Card } from '../components/common/Card';
import { Sparkline } from '../components/common/Sparkline';
import { CpuIcon } from '../components/common/Icons';

interface MetricsPageProps {
  metrics: MetricSnapshot | null;
}

export const MetricsPage: React.FC<MetricsPageProps> = ({ metrics }) => {
  const [throughputHist, setThroughputHist] = useState<number[]>([]);
  const [latencyHist, setLatencyHist] = useState<number[]>([]);
  const [cacheHist, setCacheHist] = useState<number[]>([]);
  const [walHist, setWalHist] = useState<number[]>([]);

  useEffect(() => {
    if (!metrics) return;
    setThroughputHist((prev) => [...prev.slice(-19), metrics.opsPerSec]);
    setLatencyHist((prev) => [...prev.slice(-19), metrics.writeP99Ms]);
    setCacheHist((prev) => [...prev.slice(-19), metrics.blockCacheHitRate]);
    setWalHist((prev) => [...prev.slice(-19), metrics.walActiveSegmentBytes / (1024 * 1024)]);
  }, [metrics]);

  return (
    <div className="space-y-6">
      <div className="flex flex-col sm:flex-row items-start sm:items-center justify-between gap-4 p-4 rounded-xl bg-neutral-900 border border-neutral-800">
        <div className="flex items-center gap-3">
          <div className="w-9 h-9 rounded-lg bg-teal-500/10 border border-teal-500/20 text-teal-400 flex items-center justify-center">
            <CpuIcon size={18} />
          </div>
          <div>
            <h2 className="text-sm font-bold text-neutral-100 font-mono">Engine Metrics</h2>
            <p className="text-xs text-neutral-400">Live telemetry from /api/v1/metrics</p>
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

      {!metrics ? (
        <Card title="Metrics Unavailable" subtitle="Waiting for /api/v1/metrics">
          <div className="p-8 text-center text-xs font-mono text-neutral-500">
            Metrics telemetry unavailable. No data received from /api/v1/metrics.
          </div>
        </Card>
      ) : (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-6 font-mono">
          <Card title="Request Throughput" subtitle="Operations per second">
            <div className="space-y-3 mt-1">
              <div className="text-2xl font-bold text-neutral-100">
                {metrics.opsPerSec.toLocaleString()} <span className="text-xs text-neutral-400 font-normal">ops/sec</span>
              </div>
              {throughputHist.length >= 2 && (
                <Sparkline data={throughputHist} color="#10B981" width={260} height={45} showMinMax />
              )}
              <div className="pt-2 border-t border-neutral-800/80 text-[11px] text-neutral-400 flex justify-between">
                <span>Read: {metrics.readOpsPerSec.toLocaleString()} op/s</span>
                <span>Write: {metrics.writeOpsPerSec.toLocaleString()} op/s</span>
              </div>
            </div>
          </Card>

          <Card title="Tail Latency" subtitle="P99 read and write latency">
            <div className="space-y-3 mt-1">
              <div className="text-2xl font-bold text-neutral-100">
                {metrics.writeP99Ms.toFixed(2)} <span className="text-xs text-neutral-400 font-normal">ms write p99</span>
              </div>
              {latencyHist.length >= 2 && (
                <Sparkline data={latencyHist} color="#6366F1" width={260} height={45} showMinMax />
              )}
              <div className="pt-2 border-t border-neutral-800/80 text-[11px] text-neutral-400 flex justify-between">
                <span>Read p99: {metrics.readP99Ms.toFixed(2)} ms</span>
                <span>Write p99: {metrics.writeP99Ms.toFixed(2)} ms</span>
              </div>
            </div>
          </Card>

          <Card title="Block Cache Efficiency" subtitle="Hit ratio">
            <div className="space-y-3 mt-1">
              <div className="text-2xl font-bold text-neutral-100">
                {metrics.blockCacheHitRate.toFixed(1)}% <span className="text-xs text-neutral-400 font-normal">Hit Ratio</span>
              </div>
              {cacheHist.length >= 2 && (
                <Sparkline data={cacheHist} color="#14B8A6" width={260} height={45} showMinMax />
              )}
              <div className="pt-2 border-t border-neutral-800/80 text-[11px] text-neutral-400 flex justify-between">
                <span>MemTable: {(metrics.memtableBytes / (1024 * 1024)).toFixed(1)} MB</span>
                <span>Limit: {(metrics.memtableLimitBytes / (1024 * 1024)).toFixed(1)} MB</span>
              </div>
            </div>
          </Card>

          <Card title="Active WAL Segment" subtitle="Current WAL bytes">
            <div className="space-y-3 mt-1">
              <div className="text-2xl font-bold text-neutral-100">
                {(metrics.walActiveSegmentBytes / (1024 * 1024)).toFixed(1)} <span className="text-xs text-neutral-400 font-normal">MB</span>
              </div>
              {walHist.length >= 2 && (
                <Sparkline data={walHist} color="#A855F7" width={260} height={45} showMinMax />
              )}
              <div className="pt-2 border-t border-neutral-800/80 text-[11px] text-neutral-400 flex justify-between">
                <span>LSM Total: {(metrics.lsmTotalSizeBytes / (1024 * 1024)).toFixed(1)} MB</span>
              </div>
            </div>
          </Card>

          <Card title="Disk & Connections" subtitle="Storage and network state">
            <div className="space-y-3 mt-1">
              <div className="text-2xl font-bold text-neutral-100">
                {metrics.activeConnections} <span className="text-xs text-neutral-400 font-normal">TCP conns</span>
              </div>
              <div className="space-y-1.5 pt-2 border-t border-neutral-800/80 text-[11px] text-neutral-400">
                <div className="flex justify-between">
                  <span>Disk Free:</span>
                  <span className="text-neutral-200">{(metrics.diskFreeBytes / (1024 * 1024 * 1024)).toFixed(1)} GB</span>
                </div>
                <div className="flex justify-between">
                  <span>Disk Total:</span>
                  <span className="text-neutral-200">{(metrics.diskTotalBytes / (1024 * 1024 * 1024)).toFixed(1)} GB</span>
                </div>
              </div>
            </div>
          </Card>
        </div>
      )}
    </div>
  );
};
