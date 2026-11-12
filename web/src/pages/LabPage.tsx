import { Card } from '../components/common/Card';
import { AlertCircleIcon } from '../components/common/Icons';

export const LabPage: React.FC = () => {
  return (
    <div className="space-y-6">
      <div className="p-4 rounded-xl border border-neutral-800 bg-neutral-900 flex items-start gap-3">
        <div className="w-8 h-8 rounded-full bg-neutral-800 border border-neutral-700 flex items-center justify-center text-neutral-400 flex-shrink-0 mt-0.5">
          <AlertCircleIcon size={18} />
        </div>
        <div>
          <h2 className="text-sm font-bold text-neutral-100 font-mono">Lab & Chaos</h2>
          <p className="text-xs text-neutral-400 mt-0.5">
            Workload generation and crash-recovery controls require the backend /api/v1/lab/* endpoints.
          </p>
        </div>
      </div>

      <Card title="Lab Endpoints Unavailable" subtitle="Endpoints: /api/v1/lab/workload, /api/v1/lab/crash">
        <div className="p-8 text-center text-xs font-mono text-neutral-500">
          Lab and chaos endpoints are not registered on this server.
        </div>
      </Card>
    </div>
  );
};
