import React from 'react';
import type { RaftState } from '../api/types';
import { Card } from '../components/common/Card';
import { Badge } from '../components/common/Badge';
import { StatusDot } from '../components/common/StatusDot';

interface ClusterPageProps {
  raftState: RaftState | null;
}

export const ClusterPage: React.FC<ClusterPageProps> = ({ raftState }) => {
  const peers = raftState?.peers ?? [
    {
      nodeId: 2,
      address: '10.0.1.12:9098',
      role: 'Follower',
      matchIndex: 128450,
      nextIndex: 128451,
      lagEntries: 0,
      lastHeartbeatMs: 48,
      reachable: true,
    },
    {
      nodeId: 3,
      address: '10.0.1.13:9098',
      role: 'Follower',
      matchIndex: 128448,
      nextIndex: 128449,
      lagEntries: 2,
      lastHeartbeatMs: 52,
      reachable: true,
    },
  ];

  return (
    <div className="space-y-6">
      {/* 1. Topology & Quorum Grid */}
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
        {/* SVG Circular Raft Topology Graph */}
        <Card
          title="Consensus Topology Graph"
          subtitle="SVG graph of active Raft cluster nodes and replication edges"
          className="lg:col-span-2"
        >
          <div className="flex items-center justify-center p-4">
            <svg width="420" height="260" viewBox="0 0 420 260" className="overflow-visible select-none">
              <defs>
                <linearGradient id="edgeGrad" x1="0%" y1="0%" x2="100%" y2="100%">
                  <stop offset="0%" stopColor="#10B981" stopOpacity="0.8" />
                  <stop offset="100%" stopColor="#3B82F6" stopOpacity="0.8" />
                </linearGradient>
              </defs>

              {/* Edge Node 1 -> Node 2 */}
              <line x1="210" y1="50" x2="100" y2="200" stroke="url(#edgeGrad)" strokeWidth="2" strokeDasharray="4 4" className="animate-pulse" />
              <text x="135" y="115" fill="#10B981" fontSize="10" fontFamily="monospace" textAnchor="middle">lag: 0 (48ms)</text>

              {/* Edge Node 1 -> Node 3 */}
              <line x1="210" y1="50" x2="320" y2="200" stroke="url(#edgeGrad)" strokeWidth="2" strokeDasharray="4 4" className="animate-pulse" />
              <text x="285" y="115" fill="#10B981" fontSize="10" fontFamily="monospace" textAnchor="middle">lag: 2 (52ms)</text>

              {/* Edge Node 2 <-> Node 3 (Heartbeat gossip) */}
              <line x1="100" y1="200" x2="320" y2="200" stroke="#404040" strokeWidth="1.5" strokeDasharray="2 2" />

              {/* Node 1 (Leader) */}
              <g transform="translate(210, 50)">
                <circle r="36" fill="#064E3B" stroke="#10B981" strokeWidth="3" filter="drop-shadow(0 0 12px rgba(16,185,129,0.35))" />
                <text y="-8" fill="#F9FAFB" fontSize="12" fontWeight="bold" fontFamily="monospace" textAnchor="middle">NODE 1</text>
                <text y="8" fill="#34D399" fontSize="10" fontWeight="bold" fontFamily="monospace" textAnchor="middle">LEADER</text>
                <text y="22" fill="#9CA3AF" fontSize="9" fontFamily="monospace" textAnchor="middle">Term 4</text>
              </g>

              {/* Node 2 (Follower) */}
              <g transform="translate(100, 200)">
                <circle r="30" fill="#1E3A8A" stroke="#3B82F6" strokeWidth="2" filter="drop-shadow(0 0 8px rgba(59,130,246,0.25))" />
                <text y="-6" fill="#F9FAFB" fontSize="11" fontWeight="bold" fontFamily="monospace" textAnchor="middle">NODE 2</text>
                <text y="8" fill="#93C5FD" fontSize="9" fontFamily="monospace" textAnchor="middle">FOLLOWER</text>
                <text y="20" fill="#9CA3AF" fontSize="8" fontFamily="monospace" textAnchor="middle">#128450</text>
              </g>

              {/* Node 3 (Follower) */}
              <g transform="translate(320, 200)">
                <circle r="30" fill="#1E3A8A" stroke="#3B82F6" strokeWidth="2" filter="drop-shadow(0 0 8px rgba(59,130,246,0.25))" />
                <text y="-6" fill="#F9FAFB" fontSize="11" fontWeight="bold" fontFamily="monospace" textAnchor="middle">NODE 3</text>
                <text y="8" fill="#93C5FD" fontSize="9" fontFamily="monospace" textAnchor="middle">FOLLOWER</text>
                <text y="20" fill="#9CA3AF" fontSize="8" fontFamily="monospace" textAnchor="middle">#128448</text>
              </g>
            </svg>
          </div>
        </Card>

        {/* Quorum Availability Rules Card */}
        <Card title="Quorum Consensus Health" subtitle="Consensus availability status">
          <div className="space-y-4 text-xs font-mono">
            <div className="p-3 rounded-lg bg-neutral-950 border border-neutral-800 space-y-2">
              <div className="flex justify-between">
                <span className="text-neutral-400">Cluster Size (N):</span>
                <span className="text-neutral-200 font-bold">3 Nodes</span>
              </div>
              <div className="flex justify-between">
                <span className="text-neutral-400">Quorum Threshold (Q):</span>
                <span className="text-emerald-400 font-bold">2 Nodes (⌊N/2⌋ + 1)</span>
              </div>
              <div className="flex justify-between">
                <span className="text-neutral-400">Active Connected:</span>
                <span className="text-neutral-200 font-bold">3 Nodes</span>
              </div>
              <div className="flex justify-between">
                <span className="text-neutral-400">Fault Tolerance:</span>
                <span className="text-neutral-200 font-bold">1 Node Failure OK</span>
              </div>
            </div>

            <div className="p-3 rounded-lg bg-emerald-950/20 border border-emerald-500/30 text-emerald-300 text-xs leading-relaxed">
              <strong>Linearizable Consensus Active:</strong> Writes are replicated to quorum before committing. ReadIndex protocol ensures linearizable reads without state-machine drift.
            </div>

            <div className="p-3 rounded-lg bg-neutral-950 border border-neutral-800 text-[11px] text-neutral-400 space-y-1">
              <div><strong>Wire Protocol:</strong> Length-prefixed binary framing</div>
              <div><strong>Transport:</strong> mTLS / Zero-trust NodeID binding</div>
            </div>
          </div>
        </Card>
      </div>

      {/* 2. Peer Table */}
      <Card title="Raft Member Details" subtitle="Per-peer replication indices and network metrics">
        <div className="overflow-x-auto">
          <table className="w-full text-left font-mono text-xs">
            <thead>
              <tr className="border-b border-neutral-800 text-neutral-400">
                <th className="py-2.5 px-3">NODE ID</th>
                <th className="py-2.5 px-3">PEER ADDRESS</th>
                <th className="py-2.5 px-3">ROLE</th>
                <th className="py-2.5 px-3">NEXT INDEX</th>
                <th className="py-2.5 px-3">MATCH INDEX</th>
                <th className="py-2.5 px-3">REPLICATION LAG</th>
                <th className="py-2.5 px-3">HEARTBEAT</th>
                <th className="py-2.5 px-3 text-right">STATUS</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-800/60 text-neutral-200">
              {/* Local Node 1 */}
              <tr className="bg-emerald-950/10 hover:bg-neutral-800/30 transition-colors">
                <td className="py-3 px-3 font-bold text-emerald-400">node-1 (Local)</td>
                <td className="py-3 px-3 text-neutral-300">127.0.0.1:9099</td>
                <td className="py-3 px-3"><Badge variant="leader">Leader</Badge></td>
                <td className="py-3 px-3">128451</td>
                <td className="py-3 px-3">128450</td>
                <td className="py-3 px-3 text-emerald-400 font-semibold">0 entries (Owner)</td>
                <td className="py-3 px-3 text-neutral-400">Local loopback</td>
                <td className="py-3 px-3 text-right"><StatusDot status="healthy" text="Online" /></td>
              </tr>

              {/* Follower Peers */}
              {peers.map((peer) => (
                <tr key={peer.nodeId} className="hover:bg-neutral-800/30 transition-colors">
                  <td className="py-3 px-3 font-semibold">node-{peer.nodeId}</td>
                  <td className="py-3 px-3 text-neutral-300">{peer.address}</td>
                  <td className="py-3 px-3"><Badge variant="follower">Follower</Badge></td>
                  <td className="py-3 px-3">#{peer.nextIndex}</td>
                  <td className="py-3 px-3">#{peer.matchIndex}</td>
                  <td className="py-3 px-3 font-semibold text-neutral-200">
                    {peer.lagEntries === 0 ? (
                      <span className="text-emerald-400">Synchronized (0)</span>
                    ) : (
                      <span className="text-amber-400">{peer.lagEntries} entries behind</span>
                    )}
                  </td>
                  <td className="py-3 px-3 text-neutral-400">{peer.lastHeartbeatMs} ms ago</td>
                  <td className="py-3 px-3 text-right"><StatusDot status="healthy" text="Reachable" /></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Card>
    </div>
  );
};
