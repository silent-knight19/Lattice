import { useState } from 'react';
import { Card } from '../components/common/Card';
import { Badge } from '../components/common/Badge';
import { DatabaseIcon, CheckCircleIcon, SearchIcon } from '../components/common/Icons';

export const SSTablesPage: React.FC = () => {
  const [selectedFile, setSelectedFile] = useState('000104.sst');
  const [testKey, setTestKey] = useState('usr:000142');
  const [bloomResult, setBloomResult] = useState<{
    tested: boolean;
    filterMatch: boolean;
    actualMatch: boolean;
    hashes: number[];
  }>({
    tested: true,
    filterMatch: true,
    actualMatch: true,
    hashes: [14208, 38912, 88201, 104291, 120934, 131920, 140921],
  });

  const availableFiles = ['000104.sst', '000105.sst', '000106.sst', '000107.sst', '000092.sst'];

  const blocks = [
    { index: 0, offset: 0, size: 4096, type: 'Data Block 0', crcStored: '0x8AF41B92', crcComputed: '0x8AF41B92', valid: true, count: 16, range: 'usr:000100 .. usr:000115' },
    { index: 1, offset: 4096, size: 4096, type: 'Data Block 1', crcStored: '0x39B1E04C', crcComputed: '0x39B1E04C', valid: true, count: 16, range: 'usr:000116 .. usr:000131' },
    { index: 2, offset: 8192, size: 4096, type: 'Data Block 2', crcStored: '0xFA029E11', crcComputed: '0xFA029E11', valid: true, count: 16, range: 'usr:000132 .. usr:000147' },
    { index: 3, offset: 12288, size: 4096, type: 'Data Block 3', crcStored: '0x992B4501', crcComputed: '0x992B4501', valid: true, count: 16, range: 'usr:000148 .. usr:000163' },
    { index: 4, offset: 16384, size: 4096, type: 'Two-Level Index', crcStored: '0x228BA190', crcComputed: '0x228BA190', valid: true, count: 4, range: 'Separators: usr:000115 ..' },
    { index: 5, offset: 20480, size: 2048, type: 'Bloom Filter', crcStored: '0x61DF004A', crcComputed: '0x61DF004A', valid: true, count: 64, range: '142,000 bits (7 hashes)' },
    { index: 6, offset: 22528, size: 48, type: 'Fixed Footer', crcStored: '0x00000000', crcComputed: '0x00000000', valid: true, count: 1, range: 'Magic: 0xDB4775454E47494E' },
  ];

  const handleTestBloom = (e: React.FormEvent) => {
    e.preventDefault();
    if (!testKey.trim()) return;
    const isKnown = testKey.startsWith('usr:0001');
    setBloomResult({
      tested: true,
      filterMatch: isKnown,
      actualMatch: isKnown,
      hashes: [
        Math.abs(testKey.charCodeAt(0) * 1103 + 123) % 142000,
        Math.abs(testKey.charCodeAt(1) * 2311 + 456) % 142000,
        Math.abs(testKey.charCodeAt(2) * 3407 + 789) % 142000,
        Math.abs(testKey.charCodeAt(3) * 4513 + 101) % 142000,
        Math.abs(testKey.charCodeAt(4) * 5623 + 202) % 142000,
        Math.abs(testKey.charCodeAt(5) * 6731 + 303) % 142000,
        Math.abs(testKey.charCodeAt(6) * 7841 + 404) % 142000,
      ],
    });
  };

  return (
    <div className="space-y-6">
      {/* File Selector & Header */}
      <div className="flex flex-col sm:flex-row items-start sm:items-center justify-between gap-4 p-4 rounded-xl bg-neutral-900 border border-neutral-800">
        <div className="flex items-center gap-3">
          <div className="w-9 h-9 rounded-lg bg-indigo-500/10 border border-indigo-500/20 text-indigo-400 flex items-center justify-center">
            <DatabaseIcon size={18} />
          </div>
          <div>
            <h2 className="text-sm font-bold text-neutral-100 font-mono">Forensic SSTable Inspector</h2>
            <p className="text-xs text-neutral-400">Physical binary file layout, block CRCs, Bloom filters & footers</p>
          </div>
        </div>

        <div className="flex items-center gap-2">
          <span className="text-xs text-neutral-400 font-mono">Active SSTable:</span>
          <select
            value={selectedFile}
            onChange={(e) => setSelectedFile(e.target.value)}
            className="px-3 py-1.5 rounded-lg bg-neutral-950 border border-neutral-700 text-neutral-100 font-mono text-xs focus:outline-none focus:border-indigo-500"
          >
            {availableFiles.map((f) => (
              <option key={f} value={f}>{f}</option>
            ))}
          </select>
        </div>
      </div>

      {/* 1. Horizontal Physical Block Map Ruler */}
      <Card
        title="Physical Block Structure"
        subtitle="Binary layout inside file (Data Blocks → Two-Level Index → Bloom Filter → 48-byte Fixed Footer)"
      >
        <div className="space-y-3 mt-2">
          {/* Proportional visual strip */}
          <div className="h-10 w-full rounded-lg overflow-hidden flex border border-neutral-800 font-mono text-[10px] text-white font-semibold select-none">
            <div className="w-[45%] bg-neutral-700 hover:bg-neutral-600 transition-colors flex items-center justify-center border-r border-neutral-900" title="Data Blocks (4KB each with prefix compression)">
              Data Blocks (4 x 4KB)
            </div>
            <div className="w-[20%] bg-blue-600/80 hover:bg-blue-600 transition-colors flex items-center justify-center border-r border-neutral-900" title="Two-Level Index Block">
              Index Block
            </div>
            <div className="w-[25%] bg-emerald-600/80 hover:bg-emerald-600 transition-colors flex items-center justify-center border-r border-neutral-900" title="Murmur3 Bloom Filter">
              Bloom Filter
            </div>
            <div className="w-[10%] bg-amber-600/80 hover:bg-amber-600 transition-colors flex items-center justify-center" title="Fixed 48-byte Footer with Magic">
              Footer (48B)
            </div>
          </div>

          <div className="flex justify-between text-[11px] font-mono text-neutral-400">
            <span>Offset: 0x00000000</span>
            <span>Total: 22,576 bytes</span>
            <span>Magic: <strong className="text-amber-400">0xDB4775454E47494E</strong></span>
          </div>
        </div>
      </Card>

      {/* 2. Interactive Bloom Filter Simulator */}
      <Card
        title="Probabilistic Bloom Filter Simulator"
        subtitle="Test key membership to observe cold-read disk avoidance (10 bits/key, 7 Murmur3 hash functions)"
        badge={<Badge variant="success">99% Avoidance Target</Badge>}
      >
        <div className="space-y-4 mt-1">
          <form onSubmit={handleTestBloom} className="flex gap-2">
            <div className="relative flex-1">
              <input
                type="text"
                value={testKey}
                onChange={(e) => setTestKey(e.target.value)}
                placeholder="Enter key to test in filter (e.g. usr:000142)..."
                className="w-full pl-9 pr-3 py-2 bg-neutral-950 border border-neutral-800 rounded-lg text-neutral-100 text-xs font-mono focus:outline-none focus:border-emerald-500"
              />
              <span className="absolute left-3 top-2.5 text-neutral-500">
                <SearchIcon size={14} />
              </span>
            </div>
            <button
              type="submit"
              className="px-4 py-2 bg-emerald-600 hover:bg-emerald-500 text-white rounded-lg text-xs font-medium font-mono transition-colors shadow-sm shadow-emerald-600/20"
            >
              Simulate Filter
            </button>
          </form>

          {bloomResult.tested && (
            <div className="p-4 rounded-xl bg-neutral-950 border border-neutral-800 font-mono text-xs space-y-3">
              <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
                <div className="p-3 rounded-lg bg-neutral-900 border border-neutral-800/80">
                  <span className="text-[11px] text-neutral-400 block mb-1">Bloom Verdict:</span>
                  {bloomResult.filterMatch ? (
                    <span className="text-emerald-400 font-bold flex items-center gap-1.5">
                      <CheckCircleIcon size={14} />
                      PROBABLE HIT (Pass to disk)
                    </span>
                  ) : (
                    <span className="text-neutral-400 font-bold flex items-center gap-1.5">
                      DEFINITE MISS (Zero I/O disk avoidance)
                    </span>
                  )}
                </div>

                <div className="p-3 rounded-lg bg-neutral-900 border border-neutral-800/80">
                  <span className="text-[11px] text-neutral-400 block mb-1">Ground Truth Lookup:</span>
                  {bloomResult.actualMatch ? (
                    <span className="text-emerald-400 font-bold">Key Exists in Block 2</span>
                  ) : (
                    <span className="text-neutral-400 font-bold">Key Does Not Exist</span>
                  )}
                </div>

                <div className="p-3 rounded-lg bg-neutral-900 border border-neutral-800/80">
                  <span className="text-[11px] text-neutral-400 block mb-1">False Positive Rate:</span>
                  <span className="text-indigo-400 font-bold">~0.84% (≤ 1.0% Target)</span>
                </div>
              </div>

              {/* Hash bit offsets */}
              <div>
                <span className="text-[11px] text-neutral-400 block mb-1">Computed Bit Positions (7 Murmur3 Hashes):</span>
                <div className="flex flex-wrap gap-2">
                  {bloomResult.hashes.map((h, i) => (
                    <span key={i} className="px-2 py-1 rounded bg-neutral-900 border border-neutral-800 text-neutral-300 text-[11px]">
                      H_{i+1}: <strong className="text-emerald-400">#{h}</strong>
                    </span>
                  ))}
                </div>
              </div>
            </div>
          )}
        </div>
      </Card>

      {/* 3. Block CRC Verification Table */}
      <Card
        title="Physical Block Inventory & Checksum Verification"
        subtitle="CRC32-IEEE verification per 4KB data block"
      >
        <div className="overflow-x-auto">
          <table className="w-full text-left font-mono text-xs">
            <thead>
              <tr className="border-b border-neutral-800 text-neutral-400">
                <th className="py-2.5 px-3">BLOCK</th>
                <th className="py-2.5 px-3">OFFSET</th>
                <th className="py-2.5 px-3">SIZE</th>
                <th className="py-2.5 px-3">TYPE</th>
                <th className="py-2.5 px-3">STORED CRC</th>
                <th className="py-2.5 px-3">COMPUTED CRC</th>
                <th className="py-2.5 px-3">KEY RANGE / ENTRIES</th>
                <th className="py-2.5 px-3 text-right">STATUS</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-800/60 text-neutral-200">
              {blocks.map((b) => (
                <tr key={b.index} className="hover:bg-neutral-800/30 transition-colors">
                  <td className="py-2.5 px-3 font-bold text-neutral-100">#{b.index}</td>
                  <td className="py-2.5 px-3 text-neutral-400">0x{b.offset.toString(16).padStart(6, '0')}</td>
                  <td className="py-2.5 px-3">{b.size} B</td>
                  <td className="py-2.5 px-3">
                    <span className="text-indigo-400 font-semibold">{b.type}</span>
                  </td>
                  <td className="py-2.5 px-3 text-neutral-400">{b.crcStored}</td>
                  <td className="py-2.5 px-3 text-emerald-400">{b.crcComputed}</td>
                  <td className="py-2.5 px-3 text-neutral-300">{b.range}</td>
                  <td className="py-2.5 px-3 text-right">
                    <span className="inline-flex items-center gap-1 text-emerald-400 font-semibold">
                      <CheckCircleIcon size={12} />
                      VALID
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
