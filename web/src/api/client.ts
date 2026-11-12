// Lattice Console API Client (FE-2)
// Strictly follows SEC-2 and FE-2: CSRF tokens are held in-memory ONLY.
// Zero localStorage, zero document.cookie usage.

import type {
  NodeInfo,
  HealthStatus,
  MetricSnapshot,
  LSMTreeState,
  RaftState,
  KeyEntry,
  WALSegment,
  SSTableInspection,
  SystemConfig,
} from './types';

// In-memory module-level CSRF token store (never persisted to disk/storage)
let inMemoryCSRFToken: string | null = null;

export interface APIErrorEnvelope {
  error: {
    code: string;
    message: string;
    requestId?: string;
  };
}

class APIError extends Error {
  code: string;
  requestId?: string | undefined;
  constructor(message: string, code: string, requestId?: string) {
    super(message);
    this.name = 'APIError';
    this.code = code;
    this.requestId = requestId;
  }
}

async function request<T>(
  path: string,
  options: RequestInit = {},
  timeoutMs = 8000
): Promise<T> {
  const controller = new AbortController();
  const id = setTimeout(() => controller.abort(), timeoutMs);

  const headers: Record<string, string> = {
    Accept: 'application/json',
    ...(options.headers as Record<string, string>),
  };

  // Attach CSRF token on mutating requests if present
  const method = (options.method || 'GET').toUpperCase();
  if (['POST', 'PUT', 'DELETE', 'PATCH'].includes(method)) {
    if (inMemoryCSRFToken) {
      headers['X-CSRF-Token'] = inMemoryCSRFToken;
    }
  }

  try {
    const res = await fetch(path, {
      ...options,
      headers,
      signal: controller.signal,
    });

    clearTimeout(id);

    if (!res.ok) {
      let errBody: APIErrorEnvelope | null = null;
      try {
        errBody = await res.json();
      } catch {
        // Not a JSON response
      }
      const code = errBody?.error?.code || `HTTP_${res.status}`;
      const msg = errBody?.error?.message || res.statusText || 'Request failed';
      const reqId = res.headers.get('X-Request-Id') || undefined;
      throw new APIError(msg, code, reqId);
    }

    return (await res.json()) as T;
  } catch (err: unknown) {
    clearTimeout(id);
    if (err instanceof APIError) {
      throw err;
    }
    const error = err as Error;
    if (error.name === 'AbortError') {
      throw new APIError('Request timed out', 'TIMEOUT');
    }
    throw new APIError(error.message || 'Network error', 'NETWORK_ERROR');
  }
}

// Bootstrap CSRF session
export async function fetchSession(): Promise<string | null> {
  try {
    const data = await request<{ token?: string }>('/api/v1/session');
    if (data.token) {
      inMemoryCSRFToken = data.token;
      return data.token;
    }
  } catch {
    // Session token absent or rejected, continue with unauthenticated mode
  }
  return null;
}

// Live API methods (no simulated fallbacks)
export const api = {
  async getHealth(): Promise<HealthStatus> {
    const h = await request<HealthStatus>('/api/v1/health');
    return {
      ...h,
      activeIncidents: Array.isArray(h?.activeIncidents) ? h.activeIncidents : [],
    };
  },

  async getNodeInfo(): Promise<NodeInfo> {
    return request<NodeInfo>('/api/v1/node');
  },

  async getMetrics(): Promise<MetricSnapshot> {
    return request<MetricSnapshot>('/api/v1/metrics');
  },

  async getLSMTree(): Promise<LSMTreeState> {
    const tree = await request<LSMTreeState>('/api/v1/lsm/tree');
    return {
      ...tree,
      levels: (Array.isArray(tree?.levels) ? tree.levels : []).map((lvl) => ({
        ...lvl,
        files: Array.isArray(lvl?.files) ? lvl.files : [],
      })),
    };
  },

  async getRaftState(): Promise<RaftState> {
    const state = await request<RaftState>('/api/v1/raft/status');
    return {
      ...state,
      peers: Array.isArray(state?.peers) ? state.peers : [],
      timeline: Array.isArray(state?.timeline) ? state.timeline : [],
    };
  },

  async getKeys(prefix = ''): Promise<KeyEntry[]> {
    const res = await request<KeyEntry[]>(`/api/v1/keys?prefix=${encodeURIComponent(prefix)}`);
    return Array.isArray(res) ? res : [];
  },

  async putKey(key: string, value: string): Promise<void> {
    await request('/api/v1/keys/put', {
      method: 'POST',
      body: JSON.stringify({ key, value }),
    });
  },

  async deleteKey(key: string): Promise<void> {
    await request('/api/v1/keys/delete', {
      method: 'POST',
      body: JSON.stringify({ key }),
    });
  },

  async getWALSegments(): Promise<WALSegment[]> {
    const res = await request<WALSegment[]>('/api/v1/wal/segments');
    return Array.isArray(res) ? res : [];
  },

  async inspectSSTable(filename: string): Promise<SSTableInspection> {
    const ins = await request<SSTableInspection>(`/api/v1/sstables/inspect?file=${encodeURIComponent(filename)}`);
    return {
      ...ins,
      blocks: Array.isArray(ins?.blocks) ? ins.blocks : [],
    };
  },

  async triggerCompaction(): Promise<{ success: boolean; message: string }> {
    return request('/api/v1/lsm/compact', { method: 'POST' });
  },

  async triggerFlush(): Promise<{ success: boolean; message: string }> {
    return request('/api/v1/lsm/flush', { method: 'POST' });
  },

  async getSystemConfig(): Promise<SystemConfig> {
    return request<SystemConfig>('/api/v1/config');
  },

  async execConsole(command: string): Promise<{ output: string }> {
    return request<{ output: string }>('/api/v1/console/exec', {
      method: 'POST',
      body: JSON.stringify({ command }),
    });
  },

  async startWorkload(concurrency = 8, qps = 5000, readRatio = 80): Promise<{ jobId: string; status: string; message: string }> {
    return request('/api/v1/lab/workload/start', {
      method: 'POST',
      body: JSON.stringify({ concurrency, qps, readRatio }),
    });
  },

  async getWorkloadStatus(): Promise<{ running: boolean; opsDone: number; targetQPS?: number; readRatio?: number; elapsedMs?: number }> {
    return request('/api/v1/lab/workload');
  },

  async triggerCrash(): Promise<{ success: boolean; message: string }> {
    return request('/api/v1/lab/crash', { method: 'POST' });
  },

  async getRecoveryReport(): Promise<{ segmentsReplayed: number; recordsApplied: number; recordsTruncated: number; finalSequence: number; durationMs: number; integrityVerified: boolean }> {
    return request('/api/v1/lab/recover-report');
  },

  async cleanupOrphans(): Promise<{ success: boolean; cleanedCount: number; message: string }> {
    return request('/api/v1/lab/cleanup-orphans', { method: 'POST' });
  },
};
