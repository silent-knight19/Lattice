import { useState, useEffect, useCallback } from 'react';
import type { WALSegment } from '../api/types';
import { api } from '../api/client';
import { Card } from '../components/common/Card';
import { Badge } from '../components/common/Badge';
import { FileTextIcon, RefreshCwIcon } from '../components/common/Icons';

export const WALPage: React.FC = () => {
  const [segments, setSegments] = useState<WALSegment[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const loadSegments = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.getWALSegments();
      setSegments(Array.isArray(data) ? data : []);
    } catch (err) {
      setSegments([]);
      setError(err instanceof Error ? err.message : 'Failed to load WAL segments');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    loadSegments();
  }, [loadSegments]);

  return (
    <div className="space-y-6">
      <div className="p-4 rounded-xl bg-neutral-900 border border-neutral-800 flex items-center justify-between gap-4">
        <div className="flex items-center gap-3">
          <div className="w-9 h-9 rounded-lg bg-purple-500/10 border border-purple-500/20 text-purple-400 flex items-center justify-center">
            <FileTextIcon size={18} />
          </div>
          <div>
            <h2 className="text-sm font-bold text-neutral-100 font-mono">WAL & Replay</h2>
            <p className="text-xs text-neutral-400">Write-Ahead Log segments from /api/v1/wal/segments</p>
          </div>
        </div>
        <button
          type="button"
          onClick={loadSegments}
          disabled={loading}
          className="px-3 py-1.5 rounded-lg bg-neutral-950 border border-neutral-800 hover:border-neutral-700 text-neutral-300 text-xs font-mono flex items-center gap-1.5 disabled:opacity-50"
        >
          <RefreshCwIcon size={13} className={loading ? 'animate-spin' : ''} />
          <span>Refresh</span>
        </button>
      </div>

      {error && (
        <div className="p-3 rounded-lg bg-red-950/40 border border-red-500/30 text-red-400 text-xs font-mono">
          {error}
        </div>
      )}

      <Card title="WAL Segments" subtitle="Segmented log inventory">
        {!segments ? (
          <div className="p-8 text-center text-xs font-mono text-neutral-500">
            WAL telemetry unavailable. No data received from /api/v1/wal/segments.
          </div>
        ) : segments.length === 0 ? (
          <div className="p-8 text-center text-xs font-mono text-neutral-500">
            No WAL segments reported.
          </div>
        ) : (
          <div className="overflow-x-auto font-mono text-xs">
            <table className="w-full text-left">
              <thead>
                <tr className="border-b border-neutral-800 text-neutral-400">
                  <th className="py-2.5 px-3">SEGMENT</th>
                  <th className="py-2.5 px-3">STATUS</th>
                  <th className="py-2.5 px-3">SIZE</th>
                  <th className="py-2.5 px-3">RECORDS</th>
                  <th className="py-2.5 px-3">LSN RANGE</th>
                  <th className="py-2.5 px-3 text-right">INTEGRITY</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-neutral-800/60 text-neutral-200">
                {(segments || []).map((seg) => (
                  <tr key={seg.segmentNumber} className="hover:bg-neutral-800/30">
                    <td className="py-2.5 px-3 font-semibold">{seg.filename}</td>
                    <td className="py-2.5 px-3">
                      <Badge variant={seg.status === 'active' ? 'leader' : 'neutral'}>
                        {seg.status}
                      </Badge>
                    </td>
                    <td className="py-2.5 px-3">{(seg.sizeBytes / (1024 * 1024)).toFixed(2)} MB</td>
                    <td className="py-2.5 px-3">{seg.recordCount.toLocaleString()}</td>
                    <td className="py-2.5 px-3 text-neutral-400">{seg.firstLSN} .. {seg.lastLSN}</td>
                    <td className="py-2.5 px-3 text-right">
                      <Badge variant={seg.attestationStatus === 'valid' ? 'success' : 'danger'}>
                        {seg.attestationStatus}
                      </Badge>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </div>
  );
};
