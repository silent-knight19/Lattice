import React from 'react';
import {
  ActivityIcon,
  ServerIcon,
  LayersIcon,
  DatabaseIcon,
  FileTextIcon,
  ZapIcon,
  KeyIcon,
  CpuIcon,
  TerminalIcon,
  ShieldIcon,
} from '../common/Icons';

export type NavTab =
  | 'overview'
  | 'cluster'
  | 'storage-lsm'
  | 'storage-sst'
  | 'storage-wal'
  | 'raft'
  | 'keys'
  | 'metrics'
  | 'lab'
  | 'console'
  | 'system';

interface SidebarProps {
  currentTab: NavTab;
  onSelectTab: (tab: NavTab) => void;
}

export const Sidebar: React.FC<SidebarProps> = ({ currentTab, onSelectTab }) => {
  const sections = [
    {
      group: 'HEALTH & TOPOLOGY',
      items: [
        { id: 'overview' as NavTab, label: 'Overview', icon: <ActivityIcon size={16} /> },
        { id: 'cluster' as NavTab, label: 'Cluster & Nodes', icon: <ServerIcon size={16} /> },
      ],
    },
    {
      group: 'STORAGE ENGINE',
      items: [
        { id: 'storage-lsm' as NavTab, label: 'LSM Tree Explorer', icon: <LayersIcon size={16} /> },
        { id: 'storage-sst' as NavTab, label: 'SSTable Inspector', icon: <DatabaseIcon size={16} /> },
        { id: 'storage-wal' as NavTab, label: 'WAL & Replay', icon: <FileTextIcon size={16} /> },
      ],
    },
    {
      group: 'CONSENSUS & DATA',
      items: [
        { id: 'raft' as NavTab, label: 'Raft Consensus', icon: <ZapIcon size={16} /> },
        { id: 'keys' as NavTab, label: 'Keyspace Browser', icon: <KeyIcon size={16} /> },
        { id: 'metrics' as NavTab, label: 'Engine Metrics', icon: <CpuIcon size={16} /> },
      ],
    },
    {
      group: 'DIAGNOSTICS & LAB',
      items: [
        { id: 'lab' as NavTab, label: 'Lab & Chaos', icon: <ZapIcon size={16} /> },
        { id: 'console' as NavTab, label: 'Console REPL', icon: <TerminalIcon size={16} /> },
        { id: 'system' as NavTab, label: 'System & Security', icon: <ShieldIcon size={16} /> },
      ],
    },
  ];

  return (
    <aside className="w-64 bg-neutral-950 border-r border-neutral-800 flex flex-col h-screen select-none">
      {/* Brand & Engine header */}
      <div className="p-5 border-b border-neutral-800/80 flex items-center gap-3">
        <img
          src="/logo.png"
          alt="Lattice Logo"
          className="w-9 h-9 rounded-lg object-cover border border-neutral-800 shadow-lg shadow-emerald-500/15"
        />
        <div>
          <div className="flex items-center gap-1.5">
            <h1 className="text-sm font-bold tracking-tight text-neutral-100">LATTICE</h1>
            <span className="text-[10px] font-mono px-1.5 py-0.2 rounded bg-neutral-800 text-neutral-300 font-medium">v1.1</span>
          </div>
          <p className="text-[11px] text-neutral-400 font-mono">Distributed Storage Engine</p>
        </div>
      </div>

      {/* Nav groups */}
      <nav className="flex-1 overflow-y-auto p-3 space-y-5">
        {sections.map((sec) => (
          <div key={sec.group}>
            <div className="px-3 mb-1.5 text-[10px] font-semibold tracking-wider text-neutral-500 font-mono">
              {sec.group}
            </div>
            <div className="space-y-0.5">
              {sec.items.map((item) => {
                const isActive = currentTab === item.id;
                return (
                  <button
                    key={item.id}
                    type="button"
                    onClick={() => onSelectTab(item.id)}
                    className={`w-full flex items-center gap-2.5 px-3 py-2 rounded-lg text-xs font-medium transition-all ${
                      isActive
                        ? 'bg-neutral-800 text-emerald-400 shadow-sm border border-neutral-700/60 font-semibold'
                        : 'text-neutral-400 hover:text-neutral-200 hover:bg-neutral-900'
                    }`}
                  >
                    <span className={isActive ? 'text-emerald-400' : 'text-neutral-500'}>
                      {item.icon}
                    </span>
                    <span>{item.label}</span>
                  </button>
                );
              })}
            </div>
          </div>
        ))}
      </nav>

      {/* Engine Status pill at bottom */}
      <div className="p-3 border-t border-neutral-800/80 bg-neutral-950/60">
        <div className="p-2.5 rounded-lg bg-neutral-900 border border-neutral-800 flex items-center justify-between">
          <div className="flex items-center gap-2">
            <span className="w-2 h-2 rounded-full bg-emerald-500 shadow-sm shadow-emerald-500/50" />
            <span className="text-[11px] font-mono text-neutral-300">Engine Active</span>
          </div>
          <span className="text-[10px] font-mono text-neutral-500">Go 1.23</span>
        </div>
      </div>
    </aside>
  );
};
