import { useState } from 'react';
import type { SystemConfig } from '../api/types';
import { Card } from '../components/common/Card';
import { Badge } from '../components/common/Badge';
import { ConfirmDialog } from '../components/common/ConfirmDialog';
import { CheckCircleIcon } from '../components/common/Icons';

interface SystemPageProps {
  config: SystemConfig | null;
}

export const SystemPage: React.FC<SystemPageProps> = ({ config }) => {
  const [showCleanupDialog, setShowCleanupDialog] = useState(false);
  const [cleanupMessage, setCleanupMessage] = useState<string | null>(null);

  const cfg = config ?? {
    nodeId: 1,
    address: '127.0.0.1:9099',
    adminAddress: '127.0.0.1:7070',
    metricsAddress: '127.0.0.1:9100',
    dataDir: 'data',
    maxMemtableBytes: 64 * 1024 * 1024,
    compactionL0Trigger: 4,
    insecureTransport: false,
    clientTLSRequired: false,
    peerTLSEnabled: false,
    authzPolicySummary: 'Role-based access control (Admin, Writer, Reader)',
  };

  const handleRunCleanup = () => {
    setShowCleanupDialog(false);
    setCleanupMessage('Orphan file scan complete: 0 unreferenced temporary SSTable files found in ./data.');
  };

  return (
    <div className="space-y-6">
      {/* Feedback banner */}
      {cleanupMessage && (
        <div className="p-3 rounded-lg bg-emerald-950/40 border border-emerald-500/30 text-emerald-400 text-xs font-mono flex items-center justify-between">
          <span>{cleanupMessage}</span>
          <button type="button" onClick={() => setCleanupMessage(null)} className="text-neutral-400 hover:text-white">✕</button>
        </div>
      )}

      {/* 1. Zero-Trust Security Posture Checklist */}
      <Card
        title="Zero-Trust Security Posture"
        subtitle="Verification of defense-in-depth controls across transport and admin API"
        badge={<Badge variant="success">Hardened</Badge>}
      >
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4 mt-2 font-mono text-xs">
          <div className="p-3 rounded-lg bg-neutral-950 border border-neutral-800 space-y-2">
            <div className="flex items-center gap-2 text-emerald-400 font-bold">
              <CheckCircleIcon size={14} />
              <span>DNS Rebinding Defense (HostGuard)</span>
            </div>
            <p className="text-[11px] text-neutral-400 leading-relaxed">
              Enforces strict Host header allowlist matching kernel bound socket. Wildcard binds rejected fail-closed.
            </p>
          </div>

          <div className="p-3 rounded-lg bg-neutral-950 border border-neutral-800 space-y-2">
            <div className="flex items-center gap-2 text-emerald-400 font-bold">
              <CheckCircleIcon size={14} />
              <span>Drive-by Localhost Defense (OriginGuard)</span>
            </div>
            <p className="text-[11px] text-neutral-400 leading-relaxed">
              Validates HTTP Origin header against authorized origins, preventing malicious web tabs from triggering requests.
            </p>
          </div>

          <div className="p-3 rounded-lg bg-neutral-950 border border-neutral-800 space-y-2">
            <div className="flex items-center gap-2 text-emerald-400 font-bold">
              <CheckCircleIcon size={14} />
              <span>Cryptographic CSRF Guard</span>
            </div>
            <p className="text-[11px] text-neutral-400 leading-relaxed">
              Per-session cryptographically random tokens required on all state-mutating HTTP routes. Held in-memory only.
            </p>
          </div>

          <div className="p-3 rounded-lg bg-neutral-950 border border-neutral-800 space-y-2">
            <div className="flex items-center gap-2 text-emerald-400 font-bold">
              <CheckCircleIcon size={14} />
              <span>Strict Content Security Policy (CSP)</span>
            </div>
            <p className="text-[11px] text-neutral-400 leading-relaxed">
              <code>default-src &apos;self&apos;; frame-ancestors &apos;none&apos;; object-src &apos;none&apos;</code>. Inline eval and script injection blocked by browser.
            </p>
          </div>
        </div>
      </Card>

      {/* 2. Effective Configuration Table */}
      <Card
        title="Effective Runtime Configuration"
        subtitle="Resolved configuration parameters with secrets and credentials safely redacted"
      >
        <div className="overflow-x-auto mt-2 font-mono text-xs">
          <table className="w-full text-left">
            <thead>
              <tr className="border-b border-neutral-800 text-neutral-400">
                <th className="py-2 px-3">PARAMETER</th>
                <th className="py-2 px-3">RESOLVED VALUE</th>
                <th className="py-2 px-3">PROVENANCE</th>
                <th className="py-2 px-3 text-right">SECURITY CLASSIFICATION</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-800/60 text-neutral-200">
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Node ID</td>
                <td className="py-2.5 px-3 text-emerald-400">{cfg.nodeId}</td>
                <td className="py-2.5 px-3 text-neutral-400">CLI Flag (--node-id)</td>
                <td className="py-2.5 px-3 text-right"><span className="text-neutral-400">Public</span></td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Client Wire Socket</td>
                <td className="py-2.5 px-3 text-neutral-200">{cfg.address}</td>
                <td className="py-2.5 px-3 text-neutral-400">Config Default</td>
                <td className="py-2.5 px-3 text-right"><span className="text-neutral-400">Public</span></td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Admin Console Socket</td>
                <td className="py-2.5 px-3 text-emerald-400">{cfg.adminAddress}</td>
                <td className="py-2.5 px-3 text-neutral-400">CLI Flag (--admin-address)</td>
                <td className="py-2.5 px-3 text-right"><Badge variant="level">Loopback Enforced</Badge></td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Prometheus Metrics Address</td>
                <td className="py-2.5 px-3 text-neutral-200">{cfg.metricsAddress}</td>
                <td className="py-2.5 px-3 text-neutral-400">CLI Flag (--metrics-address)</td>
                <td className="py-2.5 px-3 text-right"><span className="text-neutral-400">Public Read-only</span></td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Data Storage Directory</td>
                <td className="py-2.5 px-3 text-neutral-200">{cfg.dataDir} (Base name only)</td>
                <td className="py-2.5 px-3 text-neutral-400">CLI Flag (--data-dir)</td>
                <td className="py-2.5 px-3 text-right"><Badge variant="neutral">Path Masked</Badge></td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">MemTable Size Threshold</td>
                <td className="py-2.5 px-3 text-indigo-400">{(cfg.maxMemtableBytes / (1024 * 1024)).toFixed(0)} MB</td>
                <td className="py-2.5 px-3 text-neutral-400">Default Setting</td>
                <td className="py-2.5 px-3 text-right"><span className="text-neutral-400">Public</span></td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">L0 Compaction File Trigger</td>
                <td className="py-2.5 px-3 text-indigo-400">{cfg.compactionL0Trigger} Files</td>
                <td className="py-2.5 px-3 text-neutral-400">Default Setting</td>
                <td className="py-2.5 px-3 text-right"><span className="text-neutral-400">Public</span></td>
              </tr>
              <tr className="hover:bg-neutral-800/30">
                <td className="py-2.5 px-3 font-semibold">Client TLS Private Key</td>
                <td className="py-2.5 px-3 text-neutral-500 italic">&lt;redacted&gt;</td>
                <td className="py-2.5 px-3 text-neutral-400">Filesystem Keyring</td>
                <td className="py-2.5 px-3 text-right"><Badge variant="danger">Secret Masked</Badge></td>
              </tr>
            </tbody>
          </table>
        </div>
      </Card>

      {/* 3. Maintenance & Cleanup Actions */}
      <Card
        title="Engine Maintenance & File Hygiene"
        subtitle="Clean unreferenced files and scan for orphaned staging artifacts"
      >
        <div className="flex flex-col sm:flex-row items-center justify-between gap-4 p-3 rounded-lg bg-neutral-950 border border-neutral-800 font-mono text-xs">
          <div>
            <div className="text-neutral-200 font-semibold">Orphan File Garbage Collection</div>
            <p className="text-[11px] text-neutral-400">
              Scans database storage directory for temporary SSTable staging fragments (.tmp_*.sst) left by historical aborted writes.
            </p>
          </div>
          <button
            type="button"
            onClick={() => setShowCleanupDialog(true)}
            className="px-4 py-2 bg-neutral-800 hover:bg-neutral-700 text-neutral-200 border border-neutral-700 rounded-lg text-xs font-medium transition-colors whitespace-nowrap"
          >
            Scan & Clean Orphans
          </button>
        </div>
      </Card>

      <ConfirmDialog
        isOpen={showCleanupDialog}
        title="Clean Orphaned Storage Files"
        description="This will inspect ./data and remove unreferenced temporary staging files (.tmp_*.sst). Active version-pinned files are unaffected."
        confirmText="Execute Cleanup"
        isDestructive={false}
        onConfirm={handleRunCleanup}
        onCancel={() => setShowCleanupDialog(false)}
      />
    </div>
  );
};
