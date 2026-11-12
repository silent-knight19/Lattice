import React from 'react';

interface SparklineProps {
  data: number[];
  width?: number;
  height?: number;
  color?: string;
  className?: string;
  showMinMax?: boolean;
}

export const Sparkline: React.FC<SparklineProps> = ({
  data,
  width = 160,
  height = 40,
  color = '#10B981', // emerald-500
  className = '',
  showMinMax = false,
}) => {
  if (!data || data.length < 2) {
    return <div className={`h-[${height}px] w-[${width}px] bg-neutral-800/30 rounded`} />;
  }

  const min = Math.min(...data);
  const max = Math.max(...data);
  const range = max - min || 1;

  const padding = 4;
  const usableWidth = width - padding * 2;
  const usableHeight = height - padding * 2;

  const points = data.map((val, idx) => {
    const x = padding + (idx / (data.length - 1)) * usableWidth;
    const y = height - padding - ((val - min) / range) * usableHeight;
    return `${x},${y}`;
  });

  const polylinePoints = points.join(' ');
  const areaPoints = `${padding},${height} ${polylinePoints} ${width - padding},${height}`;

  return (
    <div className={`relative inline-block ${className}`}>
      <svg width={width} height={height} className="overflow-visible">
        <defs>
          <linearGradient id={`grad-${color.replace('#', '')}`} x1="0%" y1="0%" x2="0%" y2="100%">
            <stop offset="0%" stopColor={color} stopOpacity="0.25" />
            <stop offset="100%" stopColor={color} stopOpacity="0.0" />
          </linearGradient>
        </defs>

        {/* Gradient fill underneath curve */}
        <polygon points={areaPoints} fill={`url(#grad-${color.replace('#', '')})`} />

        {/* The metric line */}
        <polyline
          fill="none"
          stroke={color}
          strokeWidth="1.75"
          strokeLinecap="round"
          strokeLinejoin="round"
          points={polylinePoints}
        />
      </svg>
      {showMinMax && (
        <div className="flex justify-between text-[10px] text-neutral-500 font-mono mt-1">
          <span>min: {min.toFixed(1)}</span>
          <span>max: {max.toFixed(1)}</span>
        </div>
      )}
    </div>
  );
};
