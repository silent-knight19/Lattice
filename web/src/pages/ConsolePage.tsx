import { useState, useRef, useEffect } from 'react';
import { Badge } from '../components/common/Badge';
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
  const [lines, setLines] = useState<ConsoleLine[]>([
    {
      id: 'init-1',
      type: 'output',
      text: 'Lattice Distributed Storage Engine — Interactive Console REPL v1.1',
      timestamp: '11:00:00',
    },
    {
      id: 'init-2',
      type: 'output',
      text: 'Type HELP for recognized commands (GET, PUT, DEL, STATS, CLUSTER, COMPACT)',
      timestamp: '11:00:00',
    },
  ]);

  const bottomRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [lines]);

  const executeCommand = (cmd: string) => {
    const trimmed = cmd.trim();
    if (!trimmed) return;

    const time = new Date().toTimeString().split(' ')[0];
    const newLines: ConsoleLine[] = [
      ...lines,
      { id: Math.random().toString(), type: 'input', text: `lattice> ${trimmed}`, timestamp: time },
    ];

    setHistory((prev) => [trimmed, ...prev]);
    setHistoryIndex(-1);

    const parts = trimmed.split(/\s+/);
    const verb = parts[0].toUpperCase();

    switch (verb) {
      case 'HELP':
        newLines.push({
          id: Math.random().toString(),
          type: 'output',
          text: `COMMAND REFERENCE:\n  GET <key>          - Read value and sequence metadata\n  PUT <key> <value>  - Append write through WAL & MemTable\n  DEL <key>          - Append LSM deletion tombstone\n  STATS              - Print LSM, WAL, and MemTable diagnostics\n  CLUSTER            - Inspect Raft consensus peers\n  COMPACT            - Trigger k-way leveled compaction\n  CLEAR              - Reset console window`,
          timestamp: time,
        });
        break;

      case 'CLEAR':
        setLines([]);
        setInput('');
        return;

      case 'GET':
        if (parts.length < 2) {
          newLines.push({ id: Math.random().toString(), type: 'error', text: 'Error: syntax is GET <key>', timestamp: time });
        } else {
          newLines.push({
            id: Math.random().toString(),
            type: 'output',
            text: `[OK] Key: "${parts[1]}"\nValue: "{\\"name\\":\\"Alice Zhao\\",\\"tier\\":\\"enterprise\\"}"\nOrigin: Active MemTable (Seq #128450)\nRead Latency: 0.32 ms`,
            timestamp: time,
          });
        }
        break;

      case 'PUT':
        if (parts.length < 3) {
          newLines.push({ id: Math.random().toString(), type: 'error', text: 'Error: syntax is PUT <key> <val>', timestamp: time });
        } else {
          newLines.push({
            id: Math.random().toString(),
            type: 'output',
            text: `[OK] Committed to WAL segment 000014.wal and MemTable.\nAssigned Sequence Number: #128453 (Write Latency: 1.12 ms)`,
            timestamp: time,
          });
        }
        break;

      case 'DEL':
        if (parts.length < 2) {
          newLines.push({ id: Math.random().toString(), type: 'error', text: 'Error: syntax is DEL <key>', timestamp: time });
        } else {
          newLines.push({
            id: Math.random().toString(),
            type: 'output',
            text: `[OK] Appended tombstone for "${parts[1]}" at sequence #128454. Previous revisions shadowed.`,
            timestamp: time,
          });
        }
        break;

      case 'STATS':
        newLines.push({
          id: Math.random().toString(),
          type: 'output',
          text: `LATTICE ENGINE STATS:\n  Uptime: 23.4h\n  Active MemTable: 24.8 MB / 64.0 MB\n  LSM Levels: L0 (4 files, 18.2 MB), L1 (4 files, 38.4 MB), L2 (4 files, 120.0 MB)\n  Raft Term: 4 (Leader: node-1)\n  Commit Index: 128450`,
          timestamp: time,
        });
        break;

      case 'CLUSTER':
        newLines.push({
          id: Math.random().toString(),
          type: 'output',
          text: `RAFT TOPOLOGY (3/3 QUORUM):\n  node-1: LEADER (Local 127.0.0.1:9099, Term 4)\n  node-2: FOLLOWER (10.0.1.12:9098, Lag: 0 entries, 48ms)\n  node-3: FOLLOWER (10.0.1.13:9098, Lag: 2 entries, 52ms)`,
          timestamp: time,
        });
        break;

      case 'COMPACT':
        newLines.push({
          id: Math.random().toString(),
          type: 'output',
          text: `[OK] Triggered Leveled Compaction cycle across L0 → L1.\n4 input files merged into 2 non-overlapping L1 SSTable runs (412ms).`,
          timestamp: time,
        });
        break;

      default:
        newLines.push({
          id: Math.random().toString(),
          type: 'error',
          text: `Error: unrecognized command "${verb}". Type HELP for reference.`,
          timestamp: time,
        });
    }

    setLines(newLines);
    setInput('');
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      executeCommand(input);
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      if (history.length > 0 && historyIndex < history.length - 1) {
        const next = historyIndex + 1;
        setHistoryIndex(next);
        setInput(history[next]);
      }
    } else if (e.key === 'ArrowDown') {
      e.preventDefault();
      if (historyIndex > 0) {
        const next = historyIndex - 1;
        setHistoryIndex(next);
        setInput(history[next]);
      } else if (historyIndex === 0) {
        setHistoryIndex(-1);
        setInput('');
      }
    }
  };

  return (
    <div className="space-y-6">
      {/* Quick shortcuts header */}
      <div className="flex flex-wrap items-center justify-between gap-3 p-4 rounded-xl bg-neutral-900 border border-neutral-800">
        <div className="flex items-center gap-3">
          <div className="w-9 h-9 rounded-lg bg-emerald-500/10 border border-emerald-500/20 text-emerald-400 flex items-center justify-center">
            <TerminalIcon size={18} />
          </div>
          <div>
            <h2 className="text-sm font-bold text-neutral-100 font-mono">In-Browser REPL Shell</h2>
            <p className="text-xs text-neutral-400">Direct engine commands matching CLI protocol semantics</p>
          </div>
        </div>

        {/* Quick Click Commands */}
        <div className="flex flex-wrap items-center gap-1.5 font-mono text-xs">
          <span className="text-neutral-500 text-[11px] mr-1">Quick:</span>
          {['STATS', 'CLUSTER', 'GET user:000142:profile', 'COMPACT'].map((cmd) => (
            <button
              key={cmd}
              type="button"
              onClick={() => executeCommand(cmd)}
              className="px-2.5 py-1 rounded bg-neutral-950 border border-neutral-800 hover:border-emerald-500/50 hover:text-emerald-400 text-neutral-300 text-[11px] transition-colors"
            >
              {cmd}
            </button>
          ))}
        </div>
      </div>

      {/* Terminal Window */}
      <div className="rounded-xl border border-neutral-800 bg-neutral-950 shadow-2xl overflow-hidden font-mono text-xs">
        {/* Terminal Header */}
        <div className="px-4 py-2.5 border-b border-neutral-800 bg-neutral-900/80 flex items-center justify-between select-none">
          <div className="flex items-center gap-2">
            <span className="w-2.5 h-2.5 rounded-full bg-red-500/80" />
            <span className="w-2.5 h-2.5 rounded-full bg-amber-500/80" />
            <span className="w-2.5 h-2.5 rounded-full bg-emerald-500/80" />
            <span className="ml-2 text-neutral-400 text-[11px]">lattice-cli --address 127.0.0.1:9099</span>
          </div>
          <Badge variant="leader">Interactive TTY</Badge>
        </div>

        {/* Console output buffer */}
        <div className="p-4 min-h-[380px] max-h-[500px] overflow-y-auto space-y-2">
          {lines.map((l) => (
            <div key={l.id} className="leading-relaxed">
              {l.type === 'input' ? (
                <span className="text-emerald-400 font-bold">{l.text}</span>
              ) : l.type === 'error' ? (
                <span className="text-red-400">{l.text}</span>
              ) : (
                <pre className="text-neutral-300 whitespace-pre-wrap font-mono">{l.text}</pre>
              )}
            </div>
          ))}
          <div ref={bottomRef} />
        </div>

        {/* Input prompt */}
        <div className="p-3 border-t border-neutral-800 bg-neutral-900/60 flex items-center gap-2">
          <span className="text-emerald-400 font-bold select-none">lattice&gt;</span>
          <input
            type="text"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={handleKeyDown}
            placeholder="Type command (e.g. GET key, STATS, HELP)..."
            autoFocus
            className="flex-1 bg-transparent border-none text-neutral-100 focus:outline-none font-mono text-xs placeholder:text-neutral-600"
          />
        </div>
      </div>
    </div>
  );
};
