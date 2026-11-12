import { useState } from 'react';
import type { SSTableInspection } from '../api/types';
import { api } from '../api/client';
import { Card } from '../components/common/Card';
import { Badge } from '../components/common/Badge';
import { DatabaseIcon, SearchIcon } from '../components/common/Icons';

export const SSTablesPage: React.FC = () => {
  const [filename, setFilename] = useState('');
  const [inspection, setInspection] = useState<SSTableInspection | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const handleInspect = async (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = filename.trim();
    if (!trimmed) return;
    setLoading(true);
    setError(null);
    try {
      const data = await api.inspectSSTable(trimmed);
      setInspection(data);
    } catch (err) {
      setInspection(null);
      setError(err instanceof Error ? err.message : 'Failed to inspect SSTable');
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="space-y-6">
      <div className="flex flex-col sm:flex-row items-start sm:items-center justify-between gap-4 p-4 rounded-xl bg-neutral-900 border border-neutral-800">
        <div className="flex items-center gap-3">
          <div className="w-9 h-9 rounded-lg bg-indigo-500/10 border border-indigo-500/20 text-indigo-400 flex items-center justify-center">
            <DatabaseIcon size={18} />
          </div>
          <div>
            <h2 className="text-sm font-bold text-neutral-100 font-mono">SSTable Inspector</h2>
            <p className="text-xs text-neutral-400">Inspect SSTable blocks, CRC32C checksums, and Bloom filter metadata</p>
          </div>
        </div>

        <form onSubmit={handleInspect} className="flex items-center gap-2 font-mono text-xs w-full sm:w-auto">
          <input
            type="text"
            value={filename}
            onChange={(e) => setFilename(e.target.value)}
            placeholder="e.g. 000001.sst"
            className="flex-1 sm:w-52 px-3 py-1.5 rounded-lg bg-neutral-950 border border-neutral-800 text-neutral-200 focus:outline-none focus:border-indigo-500"
          />
          <button
            type="submit"
            disabled={loading || !filename.trim()}
            className="px-3 py-1.5 rounded-lg bg-indigo-500/10 border border-indigo-500/30 text-indigo-400 hover:bg-indigo-500/20 disabled:opacity-50 flex items-center gap-1.5"
          >
            <SearchIcon size={13} />
            <span>{loading ? 'Loading...' : 'Inspect'}</span>
          </button>
        </form>
      </div>

      {error && (
        <div className="p-3 rounded-lg bg-red-950/40 border border-red-500/30 text-red-400 text-xs font-mono">
          {error}
        </div>
      )}

      {!inspection ? (
        <Card title="SSTable Details" subtitle="Endpoint: /api/v1/sstables/inspect">
          <div className="p-8 text-center text-xs font-mono text-neutral-500">
            Enter an SSTable filename above to inspect its blocks and footer.
          </div>
        </Card>
      ) : (
        <div className="space-y-6 font-mono text-xs">
          <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
            <Card className="p-4">
              <div className="text-neutral-400 text-[11px]">FILE</div>
              <div className="text-sm font-bold text-neutral-100 mt-1">{inspection.filename}</div>
            </Card>
            <Card className="p-4">
              <div className="text-neutral-400 text-[11px]">LEVEL</div>
              <div className="text-sm font-bold text-neutral-100 mt-1">L{inspection.level}</div>
            </Card>
            <Card className="p-4">
              <div className="text-neutral-400 text-[11px]">ENTRIES</div>
              <div className="text-sm font-bold text-neutral-100 mt-1">{inspection.entriesCount.toLocaleString()}</div>
            </Card>
            <Card className="p-4">
              <div className="text-neutral-400 text-[11px]">FOOTER MAGIC</div>
              <div className="text-sm font-bold text-neutral-100 mt-1">
                {inspection.magicValid ? 'Valid' : 'Invalid'} ({inspection.footer.magic})
              </div>
            </Card>
          </div>

          <Card title="Data Blocks" subtitle={`${inspection.blocks.length} blocks inspected`}>
            <div className="overflow-x-auto">
              <table className="w-full text-left">
                <thead>
                  <tr className="border-b border-neutral-800 text-neutral-400">
                    <th className="py-2 px-3">BLOCK</th>
                    <th className="py-2 px-3">OFFSET</th>
                    <th className="py-2 px-3">SIZE</th>
                    <th className="py-2 px-3">KEY RANGE</th>
                    <th className="py-2 px-3">CRC STORED</th>
                    <th className="py-2 px-3 text-right">STATUS</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-neutral-800/60 text-neutral-200">
                  {inspection.blocks.map((b) => (
                    <tr key={b.index} className="hover:bg-neutral-800/30">
                      <td className="py-2 px-3">#{b.index}</td>
                      <td className="py-2 px-3">{b.offset}</td>
                      <td className="py-2 px-3">{b.sizeBytes} B</td>
                      <td className="py-2 px-3 text-neutral-400">{b.firstKey} .. {b.lastKey}</td>
                      <td className="py-2 px-3">{b.crcStored}</td>
                      <td className="py-2 px-3 text-right">
                        <Badge variant={b.crcValid ? 'success' : 'danger'}>
                          {b.crcValid ? 'CRC OK' : 'CRC Mismatch'}
                        </Badge>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Card>
        </div>
      )}
    </div>
  );
};
