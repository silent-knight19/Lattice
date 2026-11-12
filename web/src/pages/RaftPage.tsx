import type { RaftState } from '../api/types';
import { Card } from '../components/common/Card';
import { Badge } from '../components/common/Badge';

interface RaftPageProps {
  raftState: RaftState | null;
}

export const RaftPage: React.FC<RaftPageProps> = ({ raftState }) => {
  if (!raftState) {
    return (
      <Card title="Raft Consensus" subtitle="Live consensus state from /api/v1/raft/status">
        <div className="p-8 text-center text-xs font-mono text-neutral-500">
          Raft consensus telemetry unavailable. No data received from /api/v1/raft/status.
        </div>
      </Card>
    );
  }

  return (
    <div className="space-y-6">
      <div className="grid grid-cols-1 md:grid-cols-4 gap-4 font-mono text-xs">
        <Card className="p-4">
          <span className="text-neutral-400 block text-[11px]">ROLE</span>
          <div className="flex items-center gap-2 mt-1">
            <span className="text-lg font-bold text-neutral-100">{raftState.role}</span>
            <Badge variant={raftState.role === 'Leader' ? 'leader' : 'follower'}>
              node-{raftState.nodeId}
            </Badge>
          </div>
          <span className="text-[10px] text-neutral-400 mt-2 block">Leader ID: #{raftState.leaderId}</span>
        </Card>

        <Card className="p-4">
          <span className="text-neutral-400 block text-[11px]">RAFT TERM</span>
          <div className="text-lg font-bold text-neutral-100 mt-1">Term {raftState.term}</div>
          <span className="text-[10px] text-neutral-400 mt-2 block">Last Log Term: {raftState.lastLogTerm}</span>
        </Card>

        <Card className="p-4">
          <span className="text-neutral-400 block text-[11px]">COMMIT INDEX</span>
          <div className="text-lg font-bold text-neutral-100 mt-1">#{raftState.commitIndex.toLocaleString()}</div>
          <span className="text-[10px] text-neutral-400 mt-2 block">Last Log: #{raftState.lastLogIndex.toLocaleString()}</span>
        </Card>

        <Card className="p-4">
          <span className="text-neutral-400 block text-[11px]">READINDEX LATENCY</span>
          <div className="text-lg font-bold text-emerald-400 mt-1">{raftState.readIndexLatencyMs} ms</div>
          <span className="text-[10px] text-neutral-400 mt-2 block">Quorum Size: {raftState.quorumSize}</span>
        </Card>
      </div>

      <Card title="Consensus Term Timeline" subtitle="Historical leadership tenure across Raft terms">
        {raftState.timeline.length > 0 ? (
          <div className="space-y-3 mt-2">
            <div className="h-10 w-full rounded-lg bg-neutral-950 border border-neutral-800 p-1 flex gap-1 select-none font-mono text-[11px]">
              {raftState.timeline.map((t) => (
                <div
                  key={t.term}
                  className="flex-1 rounded bg-neutral-800 border border-neutral-700 flex items-center justify-center text-neutral-200 px-2 truncate"
                  title={`Term ${t.term}: Leader node-${t.leaderId}, indices ${t.startIndex}..${t.endIndex}`}
                >
                  T{t.term} (node-{t.leaderId})
                </div>
              ))}
            </div>
          </div>
        ) : (
          <div className="p-4 text-center text-xs font-mono text-neutral-500">
            No term transitions recorded.
          </div>
        )}
      </Card>
    </div>
  );
};
