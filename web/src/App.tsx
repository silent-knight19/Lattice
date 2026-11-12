import { useState, useEffect, useCallback } from 'react';
import type { NodeInfo, HealthStatus, MetricSnapshot, LSMTreeState, RaftState, SystemConfig } from './api/types';
import { api, fetchSession } from './api/client';
import { Sidebar, type NavTab } from './components/layout/Sidebar';
import { Header } from './components/layout/Header';
import { ErrorBoundary } from './components/common/ErrorBoundary';

// Feature Pages
import { OverviewPage } from './pages/OverviewPage';
import { ClusterPage } from './pages/ClusterPage';
import { LSMTreePage } from './pages/LSMTreePage';
import { SSTablesPage } from './pages/SSTablesPage';
import { WALPage } from './pages/WALPage';
import { RaftPage } from './pages/RaftPage';
import { KeysPage } from './pages/KeysPage';
import { MetricsPage } from './pages/MetricsPage';
import { LabPage } from './pages/LabPage';
import { ConsolePage } from './pages/ConsolePage';
import { SystemPage } from './pages/SystemPage';

export default function App() {
  const [currentTab, setCurrentTab] = useState<NavTab>('overview');
  const [nodeInfo, setNodeInfo] = useState<NodeInfo | null>(null);
  const [health, setHealth] = useState<HealthStatus | null>(null);
  const [metrics, setMetrics] = useState<MetricSnapshot | null>(null);
  const [lsmState, setLsmState] = useState<LSMTreeState | null>(null);
  const [raftState, setRaftState] = useState<RaftState | null>(null);
  const [config, setConfig] = useState<SystemConfig | null>(null);
  const [isRefreshing, setIsRefreshing] = useState(false);

  const loadData = useCallback(async () => {
    setIsRefreshing(true);
    try {
      const [n, h, m, l, r, c] = await Promise.allSettled([
        api.getNodeInfo(),
        api.getHealth(),
        api.getMetrics(),
        api.getLSMTree(),
        api.getRaftState(),
        api.getSystemConfig(),
      ]);

      if (n.status === 'fulfilled') setNodeInfo(n.value);
      if (h.status === 'fulfilled') setHealth(h.value);
      if (m.status === 'fulfilled') setMetrics(m.value);
      if (l.status === 'fulfilled') setLsmState(l.value);
      if (r.status === 'fulfilled') setRaftState(r.value);
      if (c.status === 'fulfilled') setConfig(c.value);
    } finally {
      setIsRefreshing(false);
    }
  }, []);

  useEffect(() => {
    // Bootstrap CSRF token on startup
    fetchSession();

    // Initial load
    loadData();

    // Periodic live telemetry refresh
    const interval = setInterval(loadData, 3000);
    return () => clearInterval(interval);
  }, [loadData]);

  const renderActivePage = () => {
    switch (currentTab) {
      case 'overview':
        return <OverviewPage nodeInfo={nodeInfo} health={health} metrics={metrics} />;
      case 'cluster':
        return <ClusterPage raftState={raftState} />;
      case 'storage-lsm':
        return <LSMTreePage treeState={lsmState} onRefresh={loadData} />;
      case 'storage-sst':
        return <SSTablesPage />;
      case 'storage-wal':
        return <WALPage />;
      case 'raft':
        return <RaftPage raftState={raftState} />;
      case 'keys':
        return <KeysPage />;
      case 'metrics':
        return <MetricsPage metrics={metrics} />;
      case 'lab':
        return <LabPage />;
      case 'console':
        return <ConsolePage />;
      case 'system':
        return <SystemPage config={config} />;
      default:
        return <OverviewPage nodeInfo={nodeInfo} health={health} metrics={metrics} />;
    }
  };

  return (
    <ErrorBoundary>
      <div className="flex h-screen bg-neutral-950 text-neutral-100 overflow-hidden font-sans">
        {/* Left-rail Navigation Sidebar */}
        <Sidebar currentTab={currentTab} onSelectTab={setCurrentTab} />

        {/* Main Content Area */}
        <div className="flex-1 flex flex-col min-w-0 overflow-hidden">
          <Header
            nodeInfo={nodeInfo}
            health={health}
            onRefresh={loadData}
            isRefreshing={isRefreshing}
          />

          <main className="flex-1 overflow-y-auto p-6 bg-gradient-to-b from-neutral-950 via-neutral-950 to-neutral-900/60">
            <div className="max-w-7xl mx-auto">
              {renderActivePage()}
            </div>
          </main>
        </div>
      </div>
    </ErrorBoundary>
  );
}
