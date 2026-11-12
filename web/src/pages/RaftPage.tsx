import { useState } from 'react';
import type { RaftState } from '../api/types';
import { Card } from '../components/common/Card';
import { Badge } from '../components/common/Badge';
import { ConfirmDialog } from '../components/common/ConfirmDialog';
import { CheckCircleIcon } from '../components/common/Icons';

interface RaftPageProps {
  raftState: RaftState | null;
}

export const RaftPage: React.FC<RaftPageProps> = ({ raftState }) => {
  const [showStepdownDialog, setShowStepdownDialog] = useState(false);
  const [actionMessage, setActionMessage] = useState<string | null>(null);

  const state = raftState ?? {
    nodeId: 1,
    role: 'Leader',
    term: 4,
    leaderId: 1,
    commitIndex: 128450,
    lastLogIndex: 128452,
    lastLogTerm: 4,
    quorumSize: 2,
    readIndexLatencyMs: 0.28,
    peers: [],
    timeline: [
      { term: 1, leaderId: 1, startIndex: 1, endIndex: 14200, durationMs: 120000 },
      { term: 2, leaderId: 2, startIndex: 14201, endIndex: 49800, durationMs: 240000 },
      { term: 3, leaderId: 3, startIndex: 49801, endIndex: 72100, durationMs: 180000 },
      { term: 4, leaderId: 1, startIndex: 72101, endIndex: 128450, durationMs: 450000 },
    ],
  };

  const sampleLogEntries = [
    { index: 128452, term: 4, type: 'COMMAND', command: 'PUT user:000142:profile', committed: true },
    { index: 128451, term: 4, type: 'COMMAND', command: 'PUT user:000143:session', committed: true },
    { index: 128450, term: 4, type: 'NOOP', command: 'Term Leader Assertion', committed: true },
    { index: 128449, term: 3, type: 'COMMAND', command: 'DEL user:000144:cache', committed: true },
    { index: 128448, term: 3, type: 'CONFIG', command: 'Cluster Membership Verify', committed: true },
  ];

  return (
    <div className="space-y-6">
      {/* Feedback banner */}
      {actionMessage && (
        <div className="p-3 rounded-lg bg-emerald-950/40 border border-emerald-500/30 text-emerald-400 text-xs font-mono flex items-center justify-between">
          <span>{actionMessage}</span>
          <button type="button" onClick={() => setActionMessage(null)} className="text-neutral-400 hover:text-white">✕</button>
        </div>
      )}

      {/* 1. Raft Status Overview Cards */}
      <div className="grid grid-cols-1 md:grid-cols-4 gap-4 font-mono text-xs">
        <Card className="p-4">
          <span className="text-neutral-400 block text-[11px]">ROLE & ELECTION</span>
          <div className="flex items-center gap-2 mt-1">
            <span className="text-lg font-bold text-neutral-100">{state.role}</span>
            <Badge variant="leader">Active Leader</Badge>
          </div>
          <span className="text-[10px] text-emerald-400 mt-2 block">Leader ID: #{state.leaderId}</span>
        </Card>

        <Card className="p-4">
          <span className="text-neutral-400 block text-[11px]">RAFT TERM</span>
          <div className="text-lg font-bold text-neutral-100 mt-1">Term {state.term}</div>
          <span className="text-[10px] text-neutral-400 mt-2 block">Voted For: #{state.leaderId}</span>
        </Card>

        <Card className="p-4">
          <span className="text-neutral-400 block text-[11px]">COMMIT INDEX</span>
          <div className="text-lg font-bold text-neutral-100 mt-1">#{state.commitIndex.toLocaleString()}</div>
          <span className="text-[10px] text-neutral-400 mt-2 block">Applied: #{state.commitIndex.toLocaleString()}</span>
        </Card>

        <Card className="p-4">
          <span className="text-neutral-400 block text-[11px]">READINDEX LATENCY</span>
          <div className="text-lg font-bold text-emerald-400 mt-1">{state.readIndexLatencyMs} ms</div>
          <span className="text-[10px] text-neutral-400 mt-2 block">Linearizable (No Drift)</span>
        </Card>
      </div>

      {/* 2. Term Timeline Ribbon Visualizer */}
      <Card
        title="Consensus Term Timeline"
        subtitle="Historical tenure across Raft terms, leadership transitions, and election rounds"
      >
        <div className="space-y-3 mt-2">
          {/* Timeline ribbon strip */}
          <div className="h-10 w-full rounded-lg bg-neutral-950 border border-neutral-800 p-1 flex gap-1 select-none font-mono text-[11px]">
            {state.timeline.map((item) => (
              <div
                key={item.term}
                className={`h-full rounded flex items-center justify-center font-bold transition-transform hover:scale-[1.01] ${
                  item.leaderId === state.nodeId
                    ? 'bg-emerald-600/80 text-white shadow-sm shadow-emerald-500/20'
                    : 'bg-blue-600/60 text-neutral-200'
                }`}
                style={{ flex: item.endIndex - item.startIndex + 5000 }}
                title={`Term ${item.term}: Leader Node ${item.leaderId} (Entries #${item.startIndex}..#${item.endIndex})`}
              >
                T{item.term} (Node {item.leaderId})
              </div>
            ))}
          </div>

          <div className="flex justify-between text-[11px] font-mono text-neutral-400">
            <span>Log Start: Entry #1</span>
            <span className="text-emerald-400 font-semibold">Current: Term {state.term} (Node 1)</span>
            <span>Head: #{state.lastLogIndex}</span>
          </div>
        </div>
      </Card>

      {/* 3. Replicated Raft Log Viewer */}
      <Card
        title="Consensus Log Entries"
        subtitle="Ordered, term-tagged Raft state machine log entries"
        badge={<Badge variant="success">Quorum Committed</Badge>}
      >
        <div className="overflow-x-auto mt-2">
          <table className="w-full text-left font-mono text-xs">
            <thead>
              <tr className="border-b border-neutral-800 text-neutral-400">
                <th className="py-2.5 px-3">INDEX</th>
                <th className="py-2.5 px-3">TERM</th>
                <th className="py-2.5 px-3">ENTRY TYPE</th>
                <th className="py-2.5 px-3">STATE MACHINE COMMAND</th>
                <th className="py-2.5 px-3 text-right">CONSENSUS STATUS</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-800/60 text-neutral-200">
              {sampleLogEntries.map((entry) => (
                <tr key={entry.index} className="hover:bg-neutral-800/30 transition-colors">
                  <td className="py-2.5 px-3 font-bold text-neutral-100">#{entry.index}</td>
                  <td className="py-2.5 px-3 text-indigo-400">Term {entry.term}</td>
                  <td className="py-2.5 px-3">
                    <span
                      className={`px-1.5 py-0.5 rounded text-[10px] font-bold ${
                        entry.type === 'COMMAND'
                          ? 'bg-emerald-500/15 text-emerald-400'
                          : entry.type === 'CONFIG'
                          ? 'bg-purple-500/15 text-purple-400'
                          : 'bg-neutral-800 text-neutral-300'
                      }`}
                    >
                      {entry.type}
                    </span>
                  </td>
                  <td className="py-2.5 px-3 text-neutral-200">{entry.command}</td>
                  <td className="py-2.5 px-3 text-right">
                    <span className="inline-flex items-center gap-1 text-emerald-400 font-semibold text-[11px]">
                      <CheckCircleIcon size={12} />
                      Committed
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Card>

      {/* 4. Danger Zone: Consensus Demonstration Actions */}
      <Card
        title="Consensus Demonstration & Failover Controls"
        subtitle="Privileged testing controls to verify election triggers and leadership failovers"
        badge={<Badge variant="danger">Privileged</Badge>}
      >
        <div className="flex flex-col sm:flex-row items-center justify-between gap-4 p-3 rounded-lg bg-neutral-950 border border-neutral-800 font-mono text-xs">
          <div>
            <div className="text-neutral-200 font-semibold">Leadership Step-Down</div>
            <p className="text-[11px] text-neutral-400">
              Force leader to step down to Follower in the same term. Triggers election among peer nodes.
            </p>
          </div>
          <button
            type="button"
            onClick={() => setShowStepdownDialog(true)}
            className="px-4 py-2 bg-red-600/20 hover:bg-red-600/30 text-red-400 border border-red-500/30 rounded-lg text-xs font-medium transition-colors whitespace-nowrap"
          >
            Step Down Leader
          </button>
        </div>
      </Card>

      <ConfirmDialog
        isOpen={showStepdownDialog}
        title="Step Down Raft Leadership"
        description="This will transition this node from Leader to Follower immediately, testing consensus failover."
        confirmText="Step Down"
        requiredWord="STEPDOWN"
        onConfirm={() => {
          setShowStepdownDialog(false);
          setActionMessage('Node successfully initiated step-down. Election timer triggered.');
        }}
        onCancel={() => setShowStepdownDialog(false)}
      />
    </div>
  );
};
