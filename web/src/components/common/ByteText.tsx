// ByteText: Central XSS-safe primitive (FE-2.4)
// Renders attacker-controlled byte strings safely.
// Printable UTF-8 characters are rendered verbatim as React text nodes.
// Non-printable, control, or binary bytes are rendered with explicit \\xHH hex escapes.

import React from 'react';

interface ByteTextProps {
  value: string | Uint8Array | null | undefined;
  className?: string;
  asMono?: boolean;
}

export const ByteText: React.FC<ByteTextProps> = ({
  value,
  className = '',
  asMono = true,
}) => {
  if (value === null || value === undefined) {
    return <span className="text-neutral-500 italic">null</span>;
  }

  const str = typeof value === 'string' ? value : new TextDecoder().decode(value);

  // Escape non-printable characters to \xHH
  let sanitized = '';
  for (let i = 0; i < str.length; i++) {
    const code = str.charCodeAt(i);
    // Printable ASCII: 32 (space) to 126 (~), plus common whitespace \n, \r, \t
    if (code >= 32 && code <= 126) {
      sanitized += str[i];
    } else if (code === 10 || code === 13 || code === 9) {
      sanitized += str[i];
    } else {
      const hex = code.toString(16).padStart(2, '0').toUpperCase();
      sanitized += `\\x${hex}`;
    }
  }

  return (
    <span
      className={`${asMono ? 'font-mono' : ''} ${className}`}
      title={sanitized}
    >
      {sanitized}
    </span>
  );
};
