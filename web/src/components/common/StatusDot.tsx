// StatusDot: Encodes status as color + icon + text (Design Spec §4 Accessibility)

import React from 'react';

export type StatusVariant = 'healthy' | 'degraded' | 'warning' | 'error' | 'recovering' | 'neutral';

interface StatusDotProps {
  status: StatusVariant;
  text?: string;
  size?: 'sm' | 'md' | 'lg';
  className?: string;
}

export const StatusDot: React.FC<StatusDotProps> = ({
  status,
  text,
  size = 'md',
  className = '',
}) => {
  const config = {
    healthy: {
      dot: 'bg-emerald-500 shadow-emerald-500/50',
      label: text || 'Healthy',
      color: 'text-emerald-400',
    },
    degraded: {
      dot: 'bg-amber-500 shadow-amber-500/50',
      label: text || 'Degraded',
      color: 'text-amber-400',
    },
    warning: {
      dot: 'bg-amber-500 shadow-amber-500/50',
      label: text || 'Warning',
      color: 'text-amber-400',
    },
    error: {
      dot: 'bg-red-500 shadow-red-500/50',
      label: text || 'Failure',
      color: 'text-red-400',
    },
    recovering: {
      dot: 'bg-blue-500 shadow-blue-500/50 animate-pulse',
      label: text || 'Recovering',
      color: 'text-blue-400',
    },
    neutral: {
      dot: 'bg-neutral-500 shadow-neutral-500/50',
      label: text || 'Idle',
      color: 'text-neutral-400',
    },
  }[status];

  const dotSize = {
    sm: 'w-1.5 h-1.5',
    md: 'w-2 h-2',
    lg: 'w-2.5 h-2.5',
  }[size];

  return (
    <span className={`inline-flex items-center gap-2 ${className}`}>
      <span className={`rounded-full shadow-sm ${dotSize} ${config.dot}`} />
      <span className={`text-xs font-medium ${config.color}`}>{config.label}</span>
    </span>
  );
};
