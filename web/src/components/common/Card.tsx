import React, { type ReactNode } from 'react';

interface CardProps {
  title?: string;
  subtitle?: string;
  badge?: ReactNode;
  action?: ReactNode;
  children: ReactNode;
  className?: string;
}

export const Card: React.FC<CardProps> = ({
  title,
  subtitle,
  badge,
  action,
  children,
  className = '',
}) => {
  return (
    <div
      className={`bg-neutral-900/90 border border-neutral-800 rounded-xl p-5 shadow-lg backdrop-blur-sm transition-colors ${className}`}
    >
      {(title || subtitle || badge || action) && (
        <div className="flex items-start justify-between mb-4 border-b border-neutral-800/80 pb-3">
          <div>
            <div className="flex items-center gap-2">
              {title && <h3 className="text-base font-semibold text-neutral-100">{title}</h3>}
              {badge}
            </div>
            {subtitle && <p className="text-xs text-neutral-400 mt-0.5">{subtitle}</p>}
          </div>
          {action && <div className="ml-4 flex-shrink-0">{action}</div>}
        </div>
      )}
      {children}
    </div>
  );
};
