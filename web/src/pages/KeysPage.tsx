import { useState, useEffect, useCallback } from 'react';
import type { KeyEntry } from '../api/types';
import { Card } from '../components/common/Card';
import { Badge } from '../components/common/Badge';
import { ByteText } from '../components/common/ByteText';
import { ConfirmDialog } from '../components/common/ConfirmDialog';
import { api } from '../api/client';
import { SearchIcon } from '../components/common/Icons';

export const KeysPage: React.FC = () => {
  const [searchTerm, setSearchTerm] = useState('');
  const [showTombstones, setShowTombstones] = useState(true);
  const [selectedEntry, setSelectedEntry] = useState<KeyEntry | null>(null);
  const [viewFormat, setViewFormat] = useState<'text' | 'json' | 'hex'>('json');

  // Mutation modal state
  const [showPutModal, setShowPutModal] = useState(false);
  const [newKey, setNewKey] = useState('');
  const [newValue, setNewValue] = useState('');
  const [putStatus, setPutStatus] = useState<{ type: 'ok' | 'err'; text: string } | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

  // Delete modal state
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);

  const [keys, setKeys] = useState<KeyEntry[]>([]);

  const loadKeys = useCallback(async (prefix: string) => {
    setLoadError(null);
    try {
      const data = await api.getKeys(prefix);
      setKeys(data);
    } catch (err) {
      setKeys([]);
      setLoadError(err instanceof Error ? err.message : 'Failed to load keys');
    }
  }, []);

  useEffect(() => {
    loadKeys(searchTerm);
  }, [loadKeys, searchTerm]);

  const filtered = keys
    .filter((k) => k.key.toLowerCase().includes(searchTerm.toLowerCase()))
    .filter((k) => showTombstones || !k.isTombstone);

  const handlePutKey = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!newKey.trim()) return;

    try {
      await api.putKey(newKey, newValue);
      setPutStatus({ type: 'ok', text: `Key "${newKey}" written.` });
      setNewKey('');
      setNewValue('');
      setShowPutModal(false);
      loadKeys(searchTerm);
    } catch (err) {
      setPutStatus({
        type: 'err',
        text: err instanceof Error ? err.message : 'Failed to write key',
      });
      setShowPutModal(false);
    }
  };

  const handleDeleteKey = async () => {
    if (!deleteTarget) return;

    try {
      await api.deleteKey(deleteTarget);
      setPutStatus({ type: 'ok', text: `Key "${deleteTarget}" deleted.` });
      setDeleteTarget(null);
      loadKeys(searchTerm);
    } catch (err) {
      setPutStatus({
        type: 'err',
        text: err instanceof Error ? err.message : 'Failed to delete key',
      });
      setDeleteTarget(null);
    }
  };

  return (
    <div className="space-y-6">
      {/* Feedback banner */}
      {putStatus && (
        <div
          className={`p-3 rounded-lg text-xs font-mono flex items-center justify-between border ${
            putStatus.type === 'ok'
              ? 'bg-emerald-950/40 border-emerald-500/30 text-emerald-400'
              : 'bg-red-950/40 border-red-500/30 text-red-400'
          }`}
        >
          <span>{putStatus.text}</span>
          <button type="button" onClick={() => setPutStatus(null)} className="text-neutral-400 hover:text-white">✕</button>
        </div>
      )}

      {loadError && (
        <div className="p-3 rounded-lg bg-red-950/40 border border-red-500/30 text-red-400 text-xs font-mono">
          {loadError}
        </div>
      )}

      {/* 1. Search Bar & Controls */}
      <div className="flex flex-col sm:flex-row items-stretch sm:items-center justify-between gap-4 p-4 rounded-xl bg-neutral-900 border border-neutral-800">
        <div className="relative flex-1">
          <input
            type="text"
            value={searchTerm}
            onChange={(e) => setSearchTerm(e.target.value)}
            placeholder="Search keys by exact match or prefix (e.g. user:0001)..."
            className="w-full pl-9 pr-3 py-2 bg-neutral-950 border border-neutral-800 rounded-lg text-neutral-100 text-xs font-mono focus:outline-none focus:border-emerald-500"
          />
          <span className="absolute left-3 top-2.5 text-neutral-500">
            <SearchIcon size={14} />
          </span>
        </div>

        <div className="flex items-center gap-3 font-mono text-xs">
          <label className="flex items-center gap-2 cursor-pointer select-none text-neutral-400">
            <input
              type="checkbox"
              checked={showTombstones}
              onChange={(e) => setShowTombstones(e.target.checked)}
              className="rounded bg-neutral-950 border-neutral-700 text-emerald-500 focus:ring-0"
            />
            <span>Show LSM Tombstones</span>
          </label>

          <button
            type="button"
            onClick={() => setShowPutModal(true)}
            className="px-4 py-2 bg-emerald-600 hover:bg-emerald-500 text-white rounded-lg font-medium transition-colors shadow-sm shadow-emerald-600/20 whitespace-nowrap"
          >
            + Put Key
          </button>
        </div>
      </div>

      {/* 2. Keyspace Table & Value Inspector */}
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
        {/* Keys List Table */}
        <Card
          title="Keyspace Domain"
          subtitle={`Displaying ${filtered.length} indexed keys across MemTable and Leveled SSTables`}
          className="lg:col-span-2"
        >
          <div className="overflow-x-auto">
            <table className="w-full text-left font-mono text-xs">
              <thead>
                <tr className="border-b border-neutral-800 text-neutral-400">
                  <th className="py-2.5 px-3">KEY</th>
                  <th className="py-2.5 px-3">LEVEL</th>
                  <th className="py-2.5 px-3">SEQ NUM</th>
                  <th className="py-2.5 px-3">SIZE</th>
                  <th className="py-2.5 px-3 text-right">ACTION</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-neutral-800/60 text-neutral-200">
                {filtered.map((entry) => {
                  const isSelected = selectedEntry?.key === entry.key;
                  return (
                    <tr
                      key={entry.key}
                      onClick={() => setSelectedEntry(entry)}
                      className={`cursor-pointer transition-colors ${
                        isSelected
                          ? 'bg-neutral-800/90 text-white font-semibold'
                          : 'hover:bg-neutral-800/30'
                      }`}
                    >
                      <td className="py-3 px-3 font-bold">
                        <div className="flex items-center gap-2">
                          <ByteText value={entry.key} />
                          {entry.isTombstone && (
                            <span className="text-[10px] px-1 py-0.2 rounded bg-red-500/20 text-red-400 border border-red-500/30 font-medium">
                              Tombstone
                            </span>
                          )}
                        </div>
                      </td>
                      <td className="py-3 px-3">
                        <Badge
                          variant={
                            entry.originLevel === 'MemTable'
                              ? 'leader'
                              : entry.originLevel === 'L0'
                              ? 'danger'
                              : 'level'
                          }
                        >
                          {entry.originLevel}
                        </Badge>
                      </td>
                      <td className="py-3 px-3 text-neutral-400">#{entry.seqNumber}</td>
                      <td className="py-3 px-3 text-neutral-400">{entry.valueSizeBytes} B</td>
                      <td className="py-3 px-3 text-right">
                        {!entry.isTombstone && (
                          <button
                            type="button"
                            onClick={(e) => {
                              e.stopPropagation();
                              setDeleteTarget(entry.key);
                            }}
                            className="px-2 py-1 text-[11px] rounded bg-red-500/10 hover:bg-red-500/20 text-red-400 border border-red-500/30 transition-colors"
                          >
                            Delete
                          </button>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </Card>

        {/* Value Inspector Card */}
        <Card
          title="Payload Inspector"
          subtitle={selectedEntry ? `Key: ${selectedEntry.key}` : 'Select a key to view value bytes'}
        >
          {selectedEntry ? (
            <div className="space-y-4 text-xs font-mono">
              <div className="flex items-center justify-between border-b border-neutral-800 pb-2">
                <span className="text-neutral-400">Representation:</span>
                <div className="flex gap-1">
                  {(['json', 'text', 'hex'] as const).map((fmt) => (
                    <button
                      key={fmt}
                      type="button"
                      onClick={() => setViewFormat(fmt)}
                      className={`px-2 py-0.5 rounded text-[10px] font-bold uppercase transition-colors ${
                        viewFormat === fmt
                          ? 'bg-emerald-600 text-white'
                          : 'bg-neutral-800 text-neutral-400 hover:text-neutral-200'
                      }`}
                    >
                      {fmt}
                    </button>
                  ))}
                </div>
              </div>

              {selectedEntry.isTombstone ? (
                <div className="p-4 rounded-lg bg-red-950/20 border border-red-500/30 text-red-400 text-center font-mono">
                  [LSM DELETION TOMBSTONE]
                  <p className="text-[11px] text-neutral-400 mt-1">
                    Value is empty. This record shadows older revisions in lower levels until leveled compaction.
                  </p>
                </div>
              ) : (
                <div className="p-3 rounded-lg bg-neutral-950 border border-neutral-800 max-h-56 overflow-y-auto">
                  <pre className="text-neutral-200 text-xs whitespace-pre-wrap break-all">
                    {viewFormat === 'hex'
                      ? Array.from(selectedEntry.value)
                          .map((c) => c.charCodeAt(0).toString(16).padStart(2, '0'))
                          .join(' ')
                      : selectedEntry.value}
                  </pre>
                </div>
              )}

              <div className="p-3 rounded-lg bg-neutral-950 border border-neutral-800 space-y-1.5 text-[11px] text-neutral-400">
                <div className="flex justify-between">
                  <span>Originating Level:</span>
                  <span className="text-neutral-200 font-bold">{selectedEntry.originLevel}</span>
                </div>
                <div className="flex justify-between">
                  <span>Sequence Number:</span>
                  <span className="text-neutral-200 font-bold">#{selectedEntry.seqNumber}</span>
                </div>
                <div className="flex justify-between">
                  <span>Timestamp:</span>
                  <span className="text-neutral-200">{selectedEntry.timestamp}</span>
                </div>
              </div>
            </div>
          ) : (
            <div className="text-center py-12 text-neutral-500 font-mono text-xs">
              No key selected. Click any row on the left to inspect its physical value.
            </div>
          )}
        </Card>
      </div>

      {/* Put Key Modal */}
      {showPutModal && (
        <div className="fixed inset-0 z-50 flex items-center justify-center p-4 bg-black/75 backdrop-blur-sm animate-in fade-in">
          <form
            onSubmit={handlePutKey}
            className="w-full max-w-md bg-neutral-900 border border-neutral-800 rounded-xl p-6 shadow-2xl space-y-4"
          >
            <h3 className="text-base font-bold text-neutral-100 font-mono">Append Key-Value Record</h3>
            <p className="text-xs text-neutral-400">
              Appends a mutation to the Write-Ahead Log (WAL) and active SkipList MemTable.
            </p>

            <div className="space-y-3 font-mono text-xs">
              <div>
                <label className="block text-neutral-400 mb-1">Key Name:</label>
                <input
                  type="text"
                  required
                  value={newKey}
                  onChange={(e) => setNewKey(e.target.value)}
                  placeholder="e.g. user:000147:settings"
                  className="w-full px-3 py-2 bg-neutral-950 border border-neutral-800 rounded-lg text-neutral-100 focus:outline-none focus:border-emerald-500"
                />
              </div>

              <div>
                <label className="block text-neutral-400 mb-1">Value Payload:</label>
                <textarea
                  rows={3}
                  required
                  value={newValue}
                  onChange={(e) => setNewValue(e.target.value)}
                  placeholder='{"status":"active","role":"admin"}'
                  className="w-full px-3 py-2 bg-neutral-950 border border-neutral-800 rounded-lg text-neutral-100 focus:outline-none focus:border-emerald-500"
                />
              </div>
            </div>

            <div className="flex justify-end gap-3 pt-2">
              <button
                type="button"
                onClick={() => setShowPutModal(false)}
                className="px-4 py-2 bg-neutral-800 hover:bg-neutral-700 text-neutral-300 text-xs font-medium rounded-lg"
              >
                Cancel
              </button>
              <button
                type="submit"
                className="px-4 py-2 bg-emerald-600 hover:bg-emerald-500 text-white text-xs font-medium rounded-lg shadow-sm shadow-emerald-600/20"
              >
                Commit Write
              </button>
            </div>
          </form>
        </div>
      )}

      {/* Delete Confirmation */}
      <ConfirmDialog
        isOpen={Boolean(deleteTarget)}
        title="Delete Key (Append Tombstone)"
        description={`This will append an LSM deletion tombstone for "${deleteTarget}" in the Active MemTable, immediately shadowing previous revisions.`}
        confirmText="Append Tombstone"
        isDestructive={true}
        onConfirm={handleDeleteKey}
        onCancel={() => setDeleteTarget(null)}
      />
    </div>
  );
};
