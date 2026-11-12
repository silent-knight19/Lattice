import { useState } from 'react';
import type { LSMTreeState } from '../api/types';
import { Card } from '../components/common/Card';
import { ByteText } from '../components/common/ByteText';
import { ConfirmDialog } from '../components/common/ConfirmDialog';
import { api } from '../api/client';
import { ZapIcon, RefreshCwIcon, AlertCircleIcon } from '../components/common/Icons';

interface LSMTreePageProps {
  treeState: LSMTreeState | null;
  onRefresh?: () => void;
}

export const LSMTreePage: React.FC<LSMTreePageProps> = ({ treeState, onRefresh }) => {
  const [isCompacting, setIsCompacting] = useState(false);
  const [isFlushing, setIsFlushing] = useState(false);
  const [showCompactDialog, setShowCompactDialog] = useState(false);
  const [showFlushDialog, setShowFlushDialog] = useState(false);
  const [feedback, setFeedback] = useState<{ type: 'ok' | 'err'; text: string } | null>(null);

  const handleManualCompaction = async () => {
    setIsCompacting(true);
    setShowCompactDialog(false);
    try {
      const res = await api.triggerCompaction();
      setFeedback({ type: 'ok', text: res.message });
      if (onRefresh) onRefresh();
    } catch (err) {
      setFeedback({ type: 'err', text: err instanceof Error ? err.message : 'Compaction request failed' });
    } finally {
      setIsCompacting(false);
    }
  };

  const handleManualFlush = async () => {
    setIsFlushing(true);
    setShowFlushDialog(false);
    try {
      const res = await api.triggerFlush();
      setFeedback({ type: 'ok', text: res.message });
      if (onRefresh) onRefresh();
    } catch (err) {
      setFeedback({ type: 'err', text: err instanceof Error ? err.message : 'Flush request failed' });
    } finally {
      setIsFlushing(false);
    }
  };

  if (!treeState) {
    return (
      <Card title="LSM Tree Explorer" subtitle="Live storage tree state from /api/v1/lsm/tree">
        <div className="p-8 text-center text-xs font-mono text-neutral-500">
          LSM tree telemetry unavailable. No data received from /api/v1/lsm/tree.
        </div>
      </Card>
    );
  }

  const { levels, memtable, compaction } = treeState;

  return (
    <div className="space-y-6">
      {/* Feedback banner */}
      {feedback && (
        <div
          className={`p-3 rounded-lg text-xs font-mono flex items-center justify-between border ${
            feedback.type === 'ok'
              ? 'bg-emerald-950/40 border-emerald-500/30 text-emerald-400'
              : 'bg-red-950/40 border-red-500/30 text-red-400'
          }`}
        >
          <span>{feedback.text}</span>
          <button type="button" onClick={() => setFeedback(null)} className="text-neutral-400 hover:text-white">✕</button>
        </div>
      )}

      {/* 1. Compaction & MemTable Header Stats */}
      <div className="grid grid-cols-1 md:grid-cols-3 gap-6">
        {/* MemTable Buffer Status */}
        <Card title="Concurrent MemTable Buffer" subtitle="Active concurrent SkipList + Immutable pipeline">
          <div className="space-y-3 mt-1 font-mono text-xs">
            <div>
              <div className="flex justify-between text-neutral-400 mb-1">
                <span>Active SkipList:</span>
                <span className="text-neutral-100 font-bold">
                  {(memtable.activeBytes / (1024 * 1024)).toFixed(1)} MB / {(memtable.flushThresholdBytes / (1024 * 1024)).toFixed(0)} MB
                </span>
              </div>
              <div className="w-full h-2 rounded-full bg-neutral-800 overflow-hidden">
                <div
                  className="h-full bg-gradient-to-r from-emerald-500 to-teal-400 rounded-full"
                  style={{ width: `${Math.min(100, (memtable.activeBytes / memtable.flushThresholdBytes) * 100)}%` }}
                />
              </div>
            </div>

            <div className="flex justify-between text-neutral-400">
              <span>Active Entries:</span>
              <span className="text-neutral-200">{memtable.activeEntries.toLocaleString()} records</span>
            </div>
            <div className="flex justify-between text-neutral-400">
              <span>Immutable MemTables:</span>
              <span className="text-amber-400 font-bold">{memtable.immutableCount} pending flush</span>
            </div>

            <button
              type="button"
              disabled={isFlushing}
              onClick={() => setShowFlushDialog(true)}
              className="w-full mt-2 py-1.5 px-3 rounded-lg bg-neutral-800 hover:bg-neutral-700 text-neutral-200 text-xs font-medium border border-neutral-700 transition-colors flex items-center justify-center gap-1.5"
            >
              <RefreshCwIcon size={12} className={isFlushing ? 'animate-spin' : ''} />
              <span>Force MemTable Flush</span>
            </button>
          </div>
        </Card>

        {/* Leveled Compactor Status */}
        <Card title="k-Way Compactor Engine" subtitle="Leveled merge compaction with 10x multiplier">
          <div className="space-y-2.5 mt-1 font-mono text-xs">
            <div className="flex justify-between">
              <span className="text-neutral-400">State:</span>
              <span className="text-emerald-400 font-bold">{compaction.running ? 'RUNNING' : 'IDLE (Score Driven)'}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-neutral-400">Total Merges:</span>
              <span className="text-neutral-200">{compaction.totalRuns} completed</span>
            </div>
            <div className="flex justify-between">
              <span className="text-neutral-400">Write Amplification:</span>
              <span className="text-indigo-400 font-bold">{compaction.writeAmplification}x</span>
            </div>
            <div className="flex justify-between">
              <span className="text-neutral-400">Last Duration:</span>
              <span className="text-neutral-200">{compaction.lastDurationMs} ms</span>
            </div>

            <button
              type="button"
              disabled={isCompacting}
              onClick={() => setShowCompactDialog(true)}
              className="w-full mt-2 py-1.5 px-3 rounded-lg bg-emerald-600/20 hover:bg-emerald-600/30 text-emerald-400 text-xs font-medium border border-emerald-500/30 transition-colors flex items-center justify-center gap-1.5"
            >
              <ZapIcon size={12} className={isCompacting ? 'animate-spin' : ''} />
              <span>Trigger Manual Compaction</span>
            </button>
          </div>
        </Card>

        {/* Compactor Rule & Info */}
        <Card title="Compaction Principles" subtitle="LSM Architectural Guarantees">
          <div className="text-xs font-mono text-neutral-300 space-y-2 leading-relaxed">
            <p>
              • <strong>Level 0:</strong> Multiple overlapping SSTables allowed. Flushed directly from MemTable.
            </p>
            <p>
              • <strong>Level 1..N:</strong> Partitioned non-overlapping runs. Binary search locates target file in O(1).
            </p>
            <p>
              • <strong>VersionSet:</strong> Atomic snapshot reads via atomic refcount (`TryRef`). Reads never block on compactions.
            </p>
          </div>
        </Card>
      </div>

      {/* 2. Visual LSM Level Columns */}
      <Card
        title="Live LSM Hierarchy & SSTable Tree"
        subtitle="Visual level hierarchy with overlapping range detection and key bounds"
      >
        <div className="grid grid-cols-1 md:grid-cols-3 gap-5 mt-2">
          {(levels || []).map((lvl) => (
            <div key={lvl.level} className="p-4 rounded-xl bg-neutral-950/80 border border-neutral-800 flex flex-col">
              <div className="flex items-center justify-between border-b border-neutral-800 pb-2.5 mb-3">
                <div className="flex items-center gap-2">
                  <span className="text-sm font-bold font-mono text-neutral-100">Level {lvl.level}</span>
                  {lvl.level === 0 ? (
                    <span className="text-[10px] font-mono px-1.5 py-0.5 rounded bg-red-500/15 text-red-400 border border-red-500/30">
                      Overlapping
                    </span>
                  ) : (
                    <span className="text-[10px] font-mono px-1.5 py-0.5 rounded bg-emerald-500/15 text-emerald-400 border border-emerald-500/30">
                      Partitioned
                    </span>
                  )}
                </div>
                <span className="text-xs font-mono text-neutral-400">
                  {((lvl.totalSizeBytes) / (1024 * 1024)).toFixed(1)} MB
                </span>
              </div>

              {/* Stack of files */}
              <div className="space-y-2.5 flex-1">
                {(lvl.files || []).map((file) => (
                  <div
                    key={file.fileNum}
                    className={`p-3 rounded-lg border transition-all ${
                      file.hasOverlap
                        ? 'bg-neutral-900 border-red-500/40 shadow-sm shadow-red-500/10'
                        : 'bg-neutral-900/90 border-neutral-800 hover:border-neutral-700'
                    }`}
                  >
                    <div className="flex items-center justify-between text-xs font-mono mb-1.5">
                      <span className="font-bold text-neutral-200">#{file.fileNum.toString().padStart(6, '0')}.sst</span>
                      <span className="text-neutral-400">{((file.sizeBytes) / (1024 * 1024)).toFixed(2)} MB</span>
                    </div>

                    <div className="text-[11px] font-mono text-neutral-400 space-y-0.5">
                      <div className="flex items-center justify-between">
                        <span>Entries:</span>
                        <span className="text-neutral-200">{file.entryCount.toLocaleString()}</span>
                      </div>
                      <div className="flex items-center justify-between">
                        <span>Range:</span>
                        <span className="text-neutral-200 truncate max-w-[140px]">
                          <ByteText value={file.smallestKey} /> .. <ByteText value={file.largestKey} />
                        </span>
                      </div>
                      <div className="flex items-center justify-between">
                        <span>SeqNum:</span>
                        <span className="text-neutral-400">#{file.smallestSeqNum} - #{file.largestSeqNum}</span>
                      </div>
                    </div>

                    {file.hasOverlap && (
                      <div className="mt-2 text-[10px] font-mono text-red-400 flex items-center gap-1 font-semibold">
                        <AlertCircleIcon size={12} />
                        <span>Key Overlap Detected (Compaction Priority)</span>
                      </div>
                    )}
                  </div>
                ))}
              </div>
            </div>
          ))}
        </div>
      </Card>

      {/* Security Confirm Dialogs */}
      <ConfirmDialog
        isOpen={showCompactDialog}
        title="Execute Leveled Compaction"
        description="This will trigger a k-way leveled merge across candidate SSTable runs. Background I/O will briefly increase."
        confirmText="Execute Compaction"
        isDestructive={false}
        onConfirm={handleManualCompaction}
        onCancel={() => setShowCompactDialog(false)}
      />

      <ConfirmDialog
        isOpen={showFlushDialog}
        title="Force Active MemTable Flush"
        description="This will seal the active SkipList MemTable and serialize it into an immutable L0 SSTable on disk."
        confirmText="Flush to Disk"
        isDestructive={false}
        onConfirm={handleManualFlush}
        onCancel={() => setShowFlushDialog(false)}
      />
    </div>
  );
};
