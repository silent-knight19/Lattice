import React from 'react';
import type { RaftState } from '../api/types';
import { Card } from '../components/common/Card';
import { Badge } from '../components/common/Badge';
import { StatusDot } from '../components/common/StatusDot';

interface ClusterPageProps {
  raftState: RaftState | null;
}

export const ClusterPage: React.FC<ClusterPageProps> = ({ raftState }) => {
  if (!raftState) {
    return (
      <Card title="Cluster & Nodes" subtitle="Live Raft cluster membership from /api/v1/raft/status">
        <div className="p-8 text-center text-xs font-mono text-neutral-500">
          Cluster telemetry unavailable. No data received from /api/v1/raft/status.
        </div>
      </Card>
    );
  }

  const totalNodes = 1 + raftState.peers.length;
  const reachablePeers = raftState.peers.filter((p) => p.reachable).length;

  return (
    <div className="space-y-6">
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
        <Card
          title="Cluster Summary"
          subtitle="Verified Raft membership state"
          className="lg:col-span-2"
        >
          <div className="grid grid-cols-2 sm:grid-cols-4 gap-4 mt-2 font-mono text-xs">
            <div className="p-3 rounded-lg bg-neutral-950 border border-neutral-800">
              <div className="text-neutral-400 text-[11px]">LOCAL NODE</div>
              <div className="text-base font-bold text-neutral-100 mt-1">node-{raftState.nodeId}</div>
              <div className="text-[11px] text-emerald-400 mt-1">{raftState.role}</div>
            </div>
            <div className="p-3 rounded-lg bg-neutral-950 border border-neutral-800">
              <div className="text-neutral-400 text-[11px]">TERM</div>
              <div className="text-base font-bold text-neutral-100 mt-1">{raftState.term}</div>
              <div className="text-[11px] text-neutral-400 mt-1">Leader: #{raftState.leaderId}</div>
            </div>
            <div className="p-3 rounded-lg bg-neutral-950 border border-neutral-800">
              <div className="text-neutral-400 text-[11px]">COMMIT INDEX</div>
              <div className="text-base font-bold text-neutral-100 mt-1">#{raftState.commitIndex}</div>
              <div className="text-[11px] text-neutral-400 mt-1">Last Log: #{raftState.lastLogIndex}</div>
            </div>
            <div className="p-3 rounded-lg bg-neutral-950 border border-neutral-800">
              <div className="text-neutral-400 text-[11px]">QUORUM</div>
              <div className="text-base font-bold text-neutral-100 mt-1">
                {1 + reachablePeers}/{totalNodes}
              </div>
              <div className="text-[11px] text-neutral-400 mt-1">Required: {raftState.quorumSize}</div>
            </div>
          </div>
        </Card>

        <Card title="Quorum Details" subtitle="Consensus configuration">
          <div className="space-y-2 text-xs font-mono p-3 rounded-lg bg-neutral-950 border border-neutral-800">
            <div className="flex justify-between">
              <span className="text-neutral-400">Cluster Size:</span>
              <span className="text-neutral-200 font-bold">{totalNodes}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-neutral-400">Quorum Threshold:</span>
              <span className="text-emerald-400 font-bold">{raftState.quorumSize}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-neutral-400">Reachable Peers:</span>
              <span className="text-neutral-200 font-bold">{reachablePeers}</span>
            </div>
          </div>
        </Card>
      </div>

      <Card title="Raft Member Details" subtitle="Per-peer replication indices">
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
              <tr className="bg-emerald-950/10">
                <td className="py-3 px-3 font-bold text-emerald-400">node-{raftState.nodeId} (Local)</td>
                <td className="py-3 px-3 text-neutral-400">—</td>
                <td className="py-3 px-3">
                  <Badge variant={raftState.role === 'Leader' ? 'leader' : 'follower'}>
                    {raftState.role}
                  </Badge>
                </td>
                <td className="py-3 px-3">—</td>
                <td className="py-3 px-3">#{raftState.commitIndex}</td>
                <td className="py-3 px-3 text-emerald-400">0 entries</td>
                <td className="py-3 px-3 text-neutral-400">Local</td>
                <td className="py-3 px-3 text-right"><StatusDot status="healthy" text="Online" /></td>
              </tr>

              {(raftState.peers || []).map((peer) => (
                <tr key={peer.nodeId} className="hover:bg-neutral-800/30 transition-colors">
                  <td className="py-3 px-3 font-semibold">node-{peer.nodeId}</td>
                  <td className="py-3 px-3 text-neutral-300">{peer.address}</td>
                  <td className="py-3 px-3">
                    <Badge variant={peer.role === 'Leader' ? 'leader' : 'follower'}>
                      {peer.role}
                    </Badge>
                  </td>
                  <td className="py-3 px-3">#{peer.nextIndex}</td>
                  <td className="py-3 px-3">#{peer.matchIndex}</td>
                  <td className="py-3 px-3 font-semibold text-neutral-200">
                    {peer.lagEntries} entries
                  </td>
                  <td className="py-3 px-3 text-neutral-400">{peer.lastHeartbeatMs} ms ago</td>
                  <td className="py-3 px-3 text-right">
                    <StatusDot
                      status={peer.reachable ? 'healthy' : 'error'}
                      text={peer.reachable ? 'Reachable' : 'Unreachable'}
                    />
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
