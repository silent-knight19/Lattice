import React from 'react';
import type { NodeInfo, HealthStatus } from '../../api/types';
import { StatusDot } from '../common/StatusDot';
import { Badge } from '../common/Badge';
import { RefreshCwIcon } from '../common/Icons';

interface HeaderProps {
  nodeInfo: NodeInfo | null;
  health: HealthStatus | null;
  onRefresh: () => void;
  isRefreshing?: boolean;
}

export const Header: React.FC<HeaderProps> = ({
  nodeInfo,
  health,
  onRefresh,
  isRefreshing = false,
}) => {
  return (
    <header className="h-14 border-b border-neutral-800 bg-neutral-950/80 backdrop-blur-md px-6 flex items-center justify-between flex-shrink-0">
      <div className="flex items-center gap-4">
        {/* Current Node Badge */}
        <div className="flex items-center gap-2">
          <span className="text-xs text-neutral-400 font-mono">Node:</span>
          <span className="text-xs font-semibold text-neutral-200 font-mono">
            {nodeInfo ? `node-${nodeInfo.nodeId}` : '—'}
          </span>
          {nodeInfo && (
            <Badge variant={nodeInfo.role === 'Leader' ? 'leader' : 'follower'}>
              {nodeInfo.role}
            </Badge>
          )}
        </div>

        <div className="h-4 w-px bg-neutral-800" />

        {/* Term & Commit */}
        <div className="hidden sm:flex items-center gap-3 text-xs font-mono text-neutral-400">
          <span>Term: <strong className="text-neutral-200">{nodeInfo ? nodeInfo.term : '—'}</strong></span>
          <span>Commit: <strong className="text-neutral-200">{nodeInfo ? `#${nodeInfo.commitIndex}` : '—'}</strong></span>
        </div>
      </div>

      <div className="flex items-center gap-4">
        {/* Overall Health indicator */}
        <StatusDot
          status={health?.status === 'healthy' ? 'healthy' : 'warning'}
          text={health ? health.status : 'Unavailable'}
        />

        <div className="h-4 w-px bg-neutral-800" />

        {/* Refresh button */}
        <button
          type="button"
          onClick={onRefresh}
          disabled={isRefreshing}
          title="Refresh Telemetry"
          className="p-1.5 rounded-lg bg-neutral-900 border border-neutral-800 text-neutral-400 hover:text-neutral-200 hover:bg-neutral-800 transition-all disabled:opacity-50"
        >
          <RefreshCwIcon size={14} className={isRefreshing ? 'animate-spin text-emerald-400' : ''} />
        </button>
      </div>
    </header>
  );
};
