// ErrorBoundary: Sanitized UI error containment (FE-3.2, SEC-9)
// Catches client exceptions without leaking raw stack traces or internal memory into the DOM.

import { Component, type ReactNode } from 'react';

interface Props {
  children: ReactNode;
}

interface State {
  hasError: boolean;
  errorId: string;
}

export class ErrorBoundary extends Component<Props, State> {
  public state: State = {
    hasError: false,
    errorId: '',
  };

  public static getDerivedStateFromError(): State {
    // Generate a per-failure correlation ID
    const randomHex = Math.random().toString(16).substring(2, 10);
    return { hasError: true, errorId: `ERR_${randomHex}` };
  }

  public componentDidCatch(error: Error): void {
    // In production we log only safe structured error envelopes
    console.error(`[Lattice Console Error] id=${this.state.errorId}`, error.message);
  }

  public render(): ReactNode {
    if (this.state.hasError) {
      return (
        <div className="min-h-[300px] flex items-center justify-center p-6">
          <div className="max-w-md w-full bg-neutral-900 border border-red-500/30 rounded-xl p-6 text-center shadow-xl">
            <div className="w-12 h-12 rounded-full bg-red-500/10 border border-red-500/20 text-red-400 flex items-center justify-center mx-auto mb-4">
              <svg className="w-6 h-6" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z" />
              </svg>
            </div>
            <h3 className="text-lg font-semibold text-neutral-100">Component Error</h3>
            <p className="mt-2 text-sm text-neutral-400">
              A rendering exception occurred. The failure has been isolated.
            </p>
            <div className="mt-4 inline-flex items-center gap-2 px-3 py-1.5 rounded bg-neutral-950 border border-neutral-800 text-xs font-mono text-neutral-400">
              <span>Incident ID:</span>
              <span className="text-neutral-200 font-semibold">{this.state.errorId}</span>
            </div>
            <div className="mt-6">
              <button
                type="button"
                onClick={() => window.location.reload()}
                className="px-4 py-2 bg-neutral-800 hover:bg-neutral-700 text-neutral-100 rounded-lg text-sm font-medium transition-colors"
              >
                Reload Console
              </button>
            </div>
          </div>
        </div>
      );
    }

    return this.props.children;
  }
}
