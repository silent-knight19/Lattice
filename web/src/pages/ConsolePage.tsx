import { useState, useRef, useEffect } from 'react';
import { api } from '../api/client';
import { TerminalIcon } from '../components/common/Icons';

interface ConsoleLine {
  id: string;
  type: 'input' | 'output' | 'error';
  text: string;
  timestamp: string;
}

export const ConsolePage: React.FC = () => {
  const [input, setInput] = useState('');
  const [history, setHistory] = useState<string[]>([]);
  const [historyIndex, setHistoryIndex] = useState(-1);
  const [running, setRunning] = useState(false);
  const [lines, setLines] = useState<ConsoleLine[]>([
    {
      id: 'init-1',
      type: 'output',
      text: 'Lattice Console — Commands are sent to POST /api/v1/console/exec',
      timestamp: new Date().toTimeString().split(' ')[0],
    },
    {
      id: 'init-2',
      type: 'output',
      text: 'Type HELP for local UI commands or CLEAR to reset.',
      timestamp: new Date().toTimeString().split(' ')[0],
    },
  ]);

  const bottomRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [lines]);

  const executeCommand = async (cmd: string) => {
    const trimmed = cmd.trim();
    if (!trimmed || running) return;

    const time = new Date().toTimeString().split(' ')[0];
    setHistory((prev) => [trimmed, ...prev]);
    setHistoryIndex(-1);
    setInput('');

    const verb = trimmed.split(/\s+/)[0].toUpperCase();
    if (verb === 'CLEAR') {
      setLines([]);
      return;
    }

    if (verb === 'HELP') {
      setLines((prev) => [
        ...prev,
        { id: `${Date.now()}-in`, type: 'input', text: `lattice> ${trimmed}`, timestamp: time },
        {
          id: `${Date.now()}-out`,
          type: 'output',
          text: 'Local commands: HELP, CLEAR\nAny other command is forwarded to POST /api/v1/console/exec.',
          timestamp: time,
        },
      ]);
      return;
    }

    setLines((prev) => [
      ...prev,
      { id: `${Date.now()}-in`, type: 'input', text: `lattice> ${trimmed}`, timestamp: time },
    ]);

    setRunning(true);
    try {
      const res = await api.execConsole(trimmed);
      setLines((prev) => [
        ...prev,
        {
          id: `${Date.now()}-out`,
          type: 'output',
          text: res.output,
          timestamp: new Date().toTimeString().split(' ')[0],
        },
      ]);
    } catch (err) {
      setLines((prev) => [
        ...prev,
        {
          id: `${Date.now()}-err`,
          type: 'error',
          text: err instanceof Error ? err.message : 'Command execution failed',
          timestamp: new Date().toTimeString().split(' ')[0],
        },
      ]);
    } finally {
      setRunning(false);
    }
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      executeCommand(input);
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      if (historyIndex < history.length - 1) {
        const nextIdx = historyIndex + 1;
        setHistoryIndex(nextIdx);
        setInput(history[nextIdx]);
      }
    } else if (e.key === 'ArrowDown') {
      e.preventDefault();
      if (historyIndex > 0) {
        const nextIdx = historyIndex - 1;
        setHistoryIndex(nextIdx);
        setInput(history[nextIdx]);
      } else if (historyIndex === 0) {
        setHistoryIndex(-1);
        setInput('');
      }
    }
  };

  return (
    <div className="space-y-4">
      <div className="rounded-xl border border-neutral-800 bg-neutral-950 overflow-hidden flex flex-col h-[560px]">
        <div className="px-4 py-2.5 bg-neutral-900 border-b border-neutral-800 flex items-center justify-between font-mono text-xs">
          <div className="flex items-center gap-2 text-neutral-300">
            <TerminalIcon size={14} className="text-emerald-400" />
            <span className="font-semibold">Lattice Console</span>
          </div>
          <span className="text-[11px] text-neutral-500">POST /api/v1/console/exec</span>
        </div>

        <div className="flex-1 overflow-y-auto p-4 space-y-2.5 font-mono text-xs">
          {lines.map((line) => (
            <div key={line.id} className="flex items-start gap-3">
              <span className="text-neutral-600 select-none flex-shrink-0">[{line.timestamp}]</span>
              <pre
                className={`whitespace-pre-wrap break-words flex-1 ${
                  line.type === 'input'
                    ? 'text-emerald-400 font-semibold'
                    : line.type === 'error'
                      ? 'text-red-400'
                      : 'text-neutral-300'
                }`}
              >
                {line.text}
              </pre>
            </div>
          ))}
          <div ref={bottomRef} />
        </div>

        <div className="p-3 bg-neutral-900/80 border-t border-neutral-800 flex items-center gap-2 font-mono text-xs">
          <span className="text-emerald-400 font-bold select-none">lattice&gt;</span>
          <input
            type="text"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={handleKeyDown}
            disabled={running}
            placeholder="Enter command..."
            className="flex-1 bg-transparent text-neutral-100 placeholder-neutral-600 focus:outline-none"
          />
        </div>
      </div>
    </div>
  );
};
