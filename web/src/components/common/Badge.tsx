import React from 'react';

export type BadgeVariant = 'leader' | 'follower' | 'candidate' | 'level' | 'success' | 'danger' | 'warning' | 'neutral';

interface BadgeProps {
  variant?: BadgeVariant;
  children: React.ReactNode;
  size?: 'sm' | 'md';
  className?: string;
}

export const Badge: React.FC<BadgeProps> = ({
  variant = 'neutral',
  children,
  size = 'sm',
  className = '',
}) => {
  const styles = {
    leader: 'bg-emerald-500/15 text-emerald-400 border-emerald-500/30',
    follower: 'bg-blue-500/15 text-blue-400 border-blue-500/30',
    candidate: 'bg-amber-500/15 text-amber-400 border-amber-500/30',
    level: 'bg-indigo-500/15 text-indigo-300 border-indigo-500/30',
    success: 'bg-emerald-500/15 text-emerald-400 border-emerald-500/30',
    danger: 'bg-red-500/15 text-red-400 border-red-500/30',
    warning: 'bg-amber-500/15 text-amber-400 border-amber-500/30',
    neutral: 'bg-neutral-800 text-neutral-300 border-neutral-700',
  }[variant];

  const sizeClasses = size === 'sm' ? 'px-2 py-0.5 text-xs' : 'px-2.5 py-1 text-sm';

  return (
    <span
      className={`inline-flex items-center font-medium border rounded-md font-mono ${sizeClasses} ${styles} ${className}`}
    >
      {children}
    </span>
  );
};
