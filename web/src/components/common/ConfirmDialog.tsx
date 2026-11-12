// ConfirmDialog: Critical Security Control (FE-4.2)
// Protects destructive operations (Compaction, Flush, Crash, Replay, Bulk Delete)
// High-severity actions require exact typed confirmation before button activates.

import React, { useState } from 'react';
import { AlertCircleIcon } from './Icons';

interface ConfirmDialogProps {
  isOpen: boolean;
  title: string;
  description: string;
  confirmText?: string;
  requiredWord?: string; // If provided, user must type this exact word to unlock
  isDestructive?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

export const ConfirmDialog: React.FC<ConfirmDialogProps> = ({
  isOpen,
  title,
  description,
  confirmText = 'Confirm',
  requiredWord,
  isDestructive = true,
  onConfirm,
  onCancel,
}) => {
  const [typedInput, setTypedInput] = useState('');

  if (!isOpen) return null;

  const isUnlocked = !requiredWord || typedInput.trim() === requiredWord.trim();

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4 bg-black/75 backdrop-blur-sm animate-in fade-in duration-150">
      <div className="w-full max-w-md bg-neutral-900 border border-neutral-800 rounded-xl p-6 shadow-2xl">
        <div className="flex items-center gap-3 mb-4">
          <div
            className={`w-10 h-10 rounded-full flex items-center justify-center ${
              isDestructive ? 'bg-red-500/10 text-red-400 border border-red-500/20' : 'bg-amber-500/10 text-amber-400 border border-amber-500/20'
            }`}
          >
            <AlertCircleIcon size={20} />
          </div>
          <div>
            <h3 className="text-base font-semibold text-neutral-100">{title}</h3>
            <span className="text-xs font-mono text-neutral-400">Privileged Operation</span>
          </div>
        </div>

        <p className="text-sm text-neutral-300 leading-relaxed mb-4">{description}</p>

        {requiredWord && (
          <div className="mb-5 p-3 rounded-lg bg-neutral-950 border border-neutral-800">
            <label htmlFor="confirm-phrase-input" className="block text-xs text-neutral-400 mb-1.5 font-medium">
              To proceed, type <span className="font-mono text-red-400 font-bold">{requiredWord}</span> below:
            </label>
            <input
              id="confirm-phrase-input"
              type="text"
              value={typedInput}
              onChange={(e) => setTypedInput(e.target.value)}
              placeholder={requiredWord}
              autoFocus
              className="w-full px-3 py-1.5 bg-neutral-900 border border-neutral-700 rounded text-neutral-100 text-sm font-mono focus:outline-none focus:border-red-500"
            />
          </div>
        )}

        <div className="flex justify-end gap-3 mt-6">
          <button
            type="button"
            onClick={() => {
              setTypedInput('');
              onCancel();
            }}
            className="px-4 py-2 bg-neutral-800 hover:bg-neutral-700 text-neutral-200 text-xs font-medium rounded-lg transition-colors"
          >
            Cancel
          </button>
          <button
            type="button"
            disabled={!isUnlocked}
            onClick={() => {
              setTypedInput('');
              onConfirm();
            }}
            className={`px-4 py-2 text-xs font-medium rounded-lg transition-all ${
              isUnlocked
                ? isDestructive
                  ? 'bg-red-600 hover:bg-red-500 text-white shadow-lg shadow-red-600/30'
                  : 'bg-emerald-600 hover:bg-emerald-500 text-white shadow-lg shadow-emerald-600/30'
                : 'bg-neutral-800 text-neutral-500 cursor-not-allowed border border-neutral-700/50'
            }`}
          >
            {confirmText}
          </button>
        </div>
      </div>
    </div>
  );
};
