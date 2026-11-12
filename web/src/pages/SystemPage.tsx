import type { SystemConfig } from '../api/types';
import { Card } from '../components/common/Card';

interface SystemPageProps {
  config: SystemConfig | null;
}

export const SystemPage: React.FC<SystemPageProps> = ({ config }) => {
  if (!config) {
    return (
      <Card title="System & Configuration" subtitle="Resolved runtime configuration from /api/v1/config">
        <div className="p-8 text-center text-xs font-mono text-neutral-500">
          System configuration unavailable. No data received from /api/v1/config.
        </div>
      </Card>
    );
  }

  return (
    <div className="space-y-6">
      <Card
        title="Runtime Configuration"
        subtitle="Resolved configuration parameters from /api/v1/config"
      >
        <div className="overflow-x-auto mt-2 font-mono text-xs">
          <table className="w-full text-left">
            <thead>
              <tr className="border-b border-neutral-800 text-neutral-400">
                <th className="py-2 px-3">PARAMETER</th>
                <th className="py-2 px-3">VALUE</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-800/60 text-neutral-200">
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Node ID</td>
                <td className="py-2.5 px-3 text-emerald-400">{config.nodeId}</td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Client Wire Address</td>
                <td className="py-2.5 px-3">{config.address}</td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Admin Address</td>
                <td className="py-2.5 px-3">{config.adminAddress}</td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Metrics Address</td>
                <td className="py-2.5 px-3">{config.metricsAddress}</td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Data Directory</td>
                <td className="py-2.5 px-3">{config.dataDir}</td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Max MemTable Bytes</td>
                <td className="py-2.5 px-3">{config.maxMemtableBytes.toLocaleString()}</td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Compaction L0 Trigger</td>
                <td className="py-2.5 px-3">{config.compactionL0Trigger}</td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Insecure Transport</td>
                <td className="py-2.5 px-3">{String(config.insecureTransport)}</td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Client TLS Required</td>
                <td className="py-2.5 px-3">{String(config.clientTLSRequired)}</td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Peer TLS Enabled</td>
                <td className="py-2.5 px-3">{String(config.peerTLSEnabled)}</td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Authz Policy</td>
                <td className="py-2.5 px-3">{config.authzPolicySummary}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </Card>
    </div>
  );
};
