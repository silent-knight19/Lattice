import { useState } from 'react';
import { Card } from '../components/common/Card';
import { Badge } from '../components/common/Badge';
import { ByteText } from '../components/common/ByteText';
import { FileTextIcon, CheckCircleIcon } from '../components/common/Icons';

export const WALPage: React.FC = () => {
  const [filterType, setFilterType] = useState<string>('ALL');

  const segments = [
    {
      num: 14,
      filename: '000014.wal',
      sizeBytes: 8.4 * 1024 * 1024,
      records: 24190,
      status: 'active',
      firstLSN: 104261,
      lastLSN: 128450,
      attestation: 'valid',
    },
    {
      num: 13,
      filename: '000013.wal',
      sizeBytes: 32.0 * 1024 * 1024,
      records: 88400,
      status: 'sealed',
      firstLSN: 15861,
      lastLSN: 104260,
      attestation: 'valid',
    },
    {
      num: 12,
      filename: '000012.wal',
      sizeBytes: 32.0 * 1024 * 1024,
      records: 91200,
      status: 'checkpointed',
      firstLSN: 1,
      lastLSN: 15860,
      attestation: 'valid',
    },
  ];

  const records = [
    { lsn: 128450, type: 'PUT', key: 'user:000142:profile', valSize: 64, crc: '0x3AF91201', time: '11:05:12.492' },
    { lsn: 128449, type: 'COMMIT', key: 'group_commit_batch', valSize: 0, crc: '0x88F021B4', time: '11:05:12.490' },
    { lsn: 128448, type: 'PUT', key: 'user:000143:session', valSize: 24, crc: '0xDF810029', time: '11:05:12.488' },
    { lsn: 128447, type: 'DELETE', key: 'user:000144:cache', valSize: 0, crc: '0x10294EA1', time: '11:05:10.120' },
    { lsn: 128446, type: 'BATCH', key: '3 operations', valSize: 184, crc: '0xAA910283', time: '11:05:08.841' },
    { lsn: 128445, type: 'CHECKPOINT', key: 'manifest_seq_110200', valSize: 48, crc: '0xFA009122', time: '11:05:00.000' },
  ];

  const filteredRecords = filterType === 'ALL'
    ? records
    : records.filter((r) => r.type === filterType);

  return (
    <div className="space-y-6">
      {/* 1. Architecture Highlights */}
      <div className="p-4 rounded-xl bg-neutral-900 border border-neutral-800 flex flex-col md:flex-row items-start md:items-center justify-between gap-4">
        <div className="flex items-center gap-3">
          <div className="w-9 h-9 rounded-lg bg-purple-500/10 border border-purple-500/20 text-purple-400 flex items-center justify-center">
            <FileTextIcon size={18} />
          </div>
          <div>
            <h2 className="text-sm font-bold text-neutral-100 font-mono">Write-Ahead Logging & Crash Forensics</h2>
            <p className="text-xs text-neutral-400">Append-only binary logs with CRC32-IEEE checksums and Group Commit barriers</p>
          </div>
        </div>
        <div className="flex items-center gap-2 font-mono text-xs">
          <Badge variant="success">fdatasync() Barrier Active</Badge>
          <Badge variant="neutral">CRC32 Attestation: 100%</Badge>
        </div>
      </div>

      {/* 2. Segments Inventory */}
      <Card title="WAL Segments on Disk" subtitle="Pre-allocated 32MB physical disk segments with SHA-256 crash attestations">
        <div className="grid grid-cols-1 md:grid-cols-3 gap-4 mt-2">
          {segments.map((seg) => (
            <div
              key={seg.num}
              className={`p-4 rounded-xl border font-mono text-xs space-y-2.5 ${
                seg.status === 'active'
                  ? 'bg-neutral-950 border-purple-500/40 shadow-sm shadow-purple-500/10'
                  : 'bg-neutral-950/60 border-neutral-800'
              }`}
            >
              <div className="flex items-center justify-between">
                <span className="font-bold text-neutral-100">{seg.filename}</span>
                {seg.status === 'active' ? (
                  <Badge variant="leader">ACTIVE APPEND</Badge>
                ) : seg.status === 'sealed' ? (
                  <Badge variant="follower">SEALED</Badge>
                ) : (
                  <Badge variant="neutral">CHECKPOINTED</Badge>
                )}
              </div>

              <div className="text-[11px] text-neutral-400 space-y-1">
                <div className="flex justify-between">
                  <span>Size:</span>
                  <span className="text-neutral-200">{(seg.sizeBytes / (1024 * 1024)).toFixed(1)} MB</span>
                </div>
                <div className="flex justify-between">
                  <span>Records:</span>
                  <span className="text-neutral-200">{seg.records.toLocaleString()}</span>
                </div>
                <div className="flex justify-between">
                  <span>LSN Range:</span>
                  <span className="text-purple-400 font-semibold">#{seg.firstLSN} .. #{seg.lastLSN}</span>
                </div>
                <div className="flex justify-between">
                  <span>Attestation:</span>
                  <span className="text-emerald-400 font-semibold">VERIFIED</span>
                </div>
              </div>
            </div>
          ))}
        </div>
      </Card>

      {/* 3. Replay Simulation & Live Record Stream */}
      <Card
        title="WAL Record Stream & Replay Simulation"
        subtitle="Ordered binary records replayed into MemTable during engine crash recovery"
        action={
          <div className="flex items-center gap-1.5 font-mono text-xs">
            <span className="text-neutral-400 mr-1">Filter:</span>
            {['ALL', 'PUT', 'DELETE', 'BATCH', 'COMMIT'].map((t) => (
              <button
                key={t}
                type="button"
                onClick={() => setFilterType(t)}
                className={`px-2 py-1 rounded text-[11px] transition-colors ${
                  filterType === t
                    ? 'bg-purple-600 text-white font-bold'
                    : 'bg-neutral-800 text-neutral-400 hover:text-neutral-200'
                }`}
              >
                {t}
              </button>
            ))}
          </div>
        }
      >
        <div className="overflow-x-auto mt-2">
          <table className="w-full text-left font-mono text-xs">
            <thead>
              <tr className="border-b border-neutral-800 text-neutral-400">
                <th className="py-2 px-3">LSN</th>
                <th className="py-2 px-3">TIMESTAMP</th>
                <th className="py-2 px-3">RECORD TYPE</th>
                <th className="py-2 px-3">KEY / PAYLOAD SUMMARY</th>
                <th className="py-2 px-3">VALUE SIZE</th>
                <th className="py-2 px-3">CRC32 CHECKSUM</th>
                <th className="py-2 px-3 text-right">REPLAY ACTION</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-800/60 text-neutral-200">
              {filteredRecords.map((r) => (
                <tr key={r.lsn} className="hover:bg-neutral-800/30 transition-colors">
                  <td className="py-2.5 px-3 font-bold text-purple-400">#{r.lsn}</td>
                  <td className="py-2.5 px-3 text-neutral-400">{r.time}</td>
                  <td className="py-2.5 px-3">
                    <span
                      className={`px-1.5 py-0.5 rounded text-[10px] font-bold ${
                        r.type === 'PUT'
                          ? 'bg-emerald-500/15 text-emerald-400'
                          : r.type === 'DELETE'
                          ? 'bg-red-500/15 text-red-400'
                          : r.type === 'COMMIT'
                          ? 'bg-blue-500/15 text-blue-400'
                          : 'bg-neutral-800 text-neutral-300'
                      }`}
                    >
                      {r.type}
                    </span>
                  </td>
                  <td className="py-2.5 px-3">
                    <ByteText value={r.key} />
                  </td>
                  <td className="py-2.5 px-3 text-neutral-400">{r.valSize} bytes</td>
                  <td className="py-2.5 px-3 text-emerald-400 font-mono">{r.crc}</td>
                  <td className="py-2.5 px-3 text-right">
                    <span className="text-[11px] text-emerald-400 font-semibold flex items-center justify-end gap-1">
                      <CheckCircleIcon size={12} />
                      Applied to SkipList
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Card>
    </div>
  );
};
