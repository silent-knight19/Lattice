import React, { useState, useEffect } from 'react';
import { Card } from '../components/common/Card';
import { ActivityIcon, RefreshCwIcon, AlertCircleIcon, CheckCircleIcon, ZapIcon } from '../components/common/Icons';
import { api } from '../api/client';

export const LabPage: React.FC = () => {
  const [concurrency, setConcurrency] = useState(8);
  const [qps, setQps] = useState(5000);
  const [readRatio, setReadRatio] = useState(80);

  const [isRunning, setIsRunning] = useState(false);
  const [opsDone, setOpsDone] = useState(0);
  const [elapsedMs, setElapsedMs] = useState(0);
  const [actionMsg, setActionMsg] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  interface RecoveryReport {
    segmentsReplayed: number;
    recordsApplied: number;
    recordsTruncated: number;
    finalSequence: number;
    durationMs: number;
    integrityVerified: boolean;
  }
  const [recoveryReport, setRecoveryReport] = useState<RecoveryReport | null>(null);

  // Poll status while running
  useEffect(() => {
    let timer: ReturnType<typeof setInterval>;
    const checkStatus = async () => {
      try {
        const stat = await api.getWorkloadStatus();
        setIsRunning(stat.running);
        setOpsDone(stat.opsDone);
        if (stat.elapsedMs) setElapsedMs(stat.elapsedMs);
      } catch {
        // silent polling catch
      }
    };

    checkStatus();
    if (isRunning) {
      timer = setInterval(checkStatus, 1000);
    }
    return () => clearInterval(timer);
  }, [isRunning]);

  const handleStartWorkload = async () => {
    setLoading(true);
    setActionMsg(null);
    try {
      const res = await api.startWorkload(concurrency, qps, readRatio);
      setIsRunning(true);
      setActionMsg(res.message);
    } catch (err: unknown) {
      setActionMsg(`Failed to start: ${(err as Error).message}`);
    } finally {
      setLoading(false);
    }
  };

  const handleTriggerCrash = async () => {
    setLoading(true);
    setActionMsg(null);
    try {
      const res = await api.triggerCrash();
      const report = await api.getRecoveryReport();
      setRecoveryReport(report);
      setActionMsg(res.message);
    } catch (err: unknown) {
      setActionMsg(`Crash simulation error: ${(err as Error).message}`);
    } finally {
      setLoading(false);
    }
  };

  const handleCleanup = async () => {
    setLoading(true);
    setActionMsg(null);
    try {
      const res = await api.cleanupOrphans();
      setActionMsg(res.message);
    } catch (err: unknown) {
      setActionMsg(`Cleanup error: ${(err as Error).message}`);
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="space-y-6">
      <div className="p-4 rounded-xl border border-neutral-800 bg-neutral-900 flex items-start justify-between">
        <div className="flex items-start gap-3">
          <div className="w-8 h-8 rounded-full bg-emerald-500/10 border border-emerald-500/20 flex items-center justify-center text-emerald-400 flex-shrink-0 mt-0.5">
            <ZapIcon size={18} />
          </div>
          <div>
            <h2 className="text-sm font-bold text-neutral-100 font-mono">Storage Engine Lab & Diagnostics</h2>
            <p className="text-xs text-neutral-400 mt-0.5">
              Live workload generator, simulated crash replay analysis, and obsolete file pruning.
            </p>
          </div>
        </div>
        {actionMsg && (
          <div className="text-xs font-mono px-3 py-1.5 rounded-md border border-neutral-700 bg-neutral-800 text-neutral-300">
            {actionMsg}
          </div>
        )}
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
        {/* Workload Generation */}
        <Card title="Workload Generator" subtitle="Generate live concurrent traffic against the storage engine">
          <div className="space-y-4">
            <div className="grid grid-cols-3 gap-3">
              <div>
                <label className="text-[10px] font-mono uppercase tracking-wider text-neutral-400 block mb-1">
                  Concurrency
                </label>
                <input
                  type="number"
                  min={1}
                  max={64}
                  value={concurrency}
                  onChange={(e) => setConcurrency(parseInt(e.target.value) || 1)}
                  disabled={isRunning}
                  className="w-full bg-neutral-950 border border-neutral-800 rounded px-2.5 py-1.5 text-xs text-neutral-100 font-mono focus:outline-none focus:border-emerald-500"
                />
              </div>
              <div>
                <label className="text-[10px] font-mono uppercase tracking-wider text-neutral-400 block mb-1">
                  Target QPS
                </label>
                <input
                  type="number"
                  min={100}
                  max={50000}
                  step={500}
                  value={qps}
                  onChange={(e) => setQps(parseInt(e.target.value) || 500)}
                  disabled={isRunning}
                  className="w-full bg-neutral-950 border border-neutral-800 rounded px-2.5 py-1.5 text-xs text-neutral-100 font-mono focus:outline-none focus:border-emerald-500"
                />
              </div>
              <div>
                <label className="text-[10px] font-mono uppercase tracking-wider text-neutral-400 block mb-1">
                  Read %
                </label>
                <input
                  type="number"
                  min={0}
                  max={100}
                  value={readRatio}
                  onChange={(e) => setReadRatio(parseInt(e.target.value) || 0)}
                  disabled={isRunning}
                  className="w-full bg-neutral-950 border border-neutral-800 rounded px-2.5 py-1.5 text-xs text-neutral-100 font-mono focus:outline-none focus:border-emerald-500"
                />
              </div>
            </div>

            <div className="p-3 rounded-lg border border-neutral-800 bg-neutral-950 flex items-center justify-between">
              <div>
                <div className="text-[10px] font-mono uppercase tracking-wider text-neutral-500">Status</div>
                <div className="text-xs font-mono font-medium flex items-center gap-1.5 mt-0.5">
                  <span className={`w-2 h-2 rounded-full ${isRunning ? 'bg-emerald-400 animate-pulse' : 'bg-neutral-600'}`} />
                  <span className={isRunning ? 'text-emerald-400' : 'text-neutral-400'}>
                    {isRunning ? 'Running' : 'Idle'}
                  </span>
                </div>
              </div>
              <div>
                <div className="text-[10px] font-mono uppercase tracking-wider text-neutral-500">Ops Executed</div>
                <div className="text-xs font-mono font-bold text-neutral-200 mt-0.5">
                  {opsDone.toLocaleString()}
                </div>
              </div>
              <div>
                <div className="text-[10px] font-mono uppercase tracking-wider text-neutral-500">Elapsed</div>
                <div className="text-xs font-mono font-medium text-neutral-200 mt-0.5">
                  {(elapsedMs / 1000).toFixed(1)}s
                </div>
              </div>
            </div>

            <button
              onClick={handleStartWorkload}
              disabled={loading || isRunning}
              className={`w-full py-2 px-3 rounded-lg font-mono text-xs font-semibold flex items-center justify-center gap-2 transition-colors ${
                isRunning
                  ? 'bg-neutral-800 text-neutral-500 cursor-not-allowed'
                  : 'bg-emerald-500 hover:bg-emerald-400 text-neutral-950 shadow-md shadow-emerald-500/20'
              }`}
            >
              <ActivityIcon size={14} />
              {isRunning ? 'Workload in Progress...' : 'Start Real Workload'}
            </button>
          </div>
        </Card>

        {/* Maintenance & Chaos */}
        <div className="space-y-6">
          <Card title="Crash & Replay Recovery" subtitle="Simulate abrupt daemon kill and verify WAL segment replay">
            <div className="space-y-3">
              <p className="text-xs text-neutral-400">
                Exercises the WAL recovery coordinator to verify that uncommitted frames are safely rolled back and committed entries are durably preserved.
              </p>

              <button
                onClick={handleTriggerCrash}
                disabled={loading}
                className="w-full py-2 px-3 rounded-lg font-mono text-xs font-semibold border border-amber-500/30 bg-amber-500/10 hover:bg-amber-500/20 text-amber-300 flex items-center justify-center gap-2 transition-colors"
              >
                <AlertCircleIcon size={14} />
                Run Crash Replay Diagnostic
              </button>

              {recoveryReport && (
                <div className="p-3 rounded-lg border border-neutral-800 bg-neutral-950 space-y-2 mt-3 text-xs font-mono">
                  <div className="flex items-center justify-between text-neutral-400">
                    <span>Replay Status:</span>
                    <span className="flex items-center gap-1 text-emerald-400 font-bold">
                      <CheckCircleIcon size={12} />
                      {recoveryReport.integrityVerified ? 'Verified Consistent' : 'Integrity Issue'}
                    </span>
                  </div>
                  <div className="flex items-center justify-between text-neutral-400">
                    <span>Segments Replayed:</span>
                    <span className="text-neutral-200">{recoveryReport.segmentsReplayed}</span>
                  </div>
                  <div className="flex items-center justify-between text-neutral-400">
                    <span>Records Applied:</span>
                    <span className="text-neutral-200">{recoveryReport.recordsApplied.toLocaleString()}</span>
                  </div>
                  <div className="flex items-center justify-between text-neutral-400">
                    <span>Final Sequence:</span>
                    <span className="text-neutral-200">#{recoveryReport.finalSequence}</span>
                  </div>
                  <div className="flex items-center justify-between text-neutral-400">
                    <span>Recovery Duration:</span>
                    <span className="text-neutral-200">{recoveryReport.durationMs} ms</span>
                  </div>
                </div>
              )}
            </div>
          </Card>

          <Card title="Orphan SSTable Pruning" subtitle="Clean dead and obsolete files not referenced in VersionSet">
            <div className="space-y-3">
              <p className="text-xs text-neutral-400">
                Safely inspects the LSM Manifest and removes obsolete SSTable files that have been merged by compaction.
              </p>
              <button
                onClick={handleCleanup}
                disabled={loading}
                className="w-full py-2 px-3 rounded-lg font-mono text-xs font-semibold border border-neutral-700 bg-neutral-800 hover:bg-neutral-700 text-neutral-200 flex items-center justify-center gap-2 transition-colors"
              >
                <RefreshCwIcon size={14} />
                Prune Obsolete Files
              </button>
            </div>
          </Card>
        </div>
      </div>
    </div>
  );
};
