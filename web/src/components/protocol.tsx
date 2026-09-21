// Shared protocol display helpers: one color map and one display order for
// every place the 'anthropic'/'openai'/'responses' enum is rendered. Protocol
// values are business enums and stay untranslated (see i18n/dictionaries.ts
// header).

import { Tag } from 'antd';
import type { TagProps } from 'antd';

export type Protocol = 'openai' | 'anthropic' | 'responses';

// Antd preset colors adapt to dark mode via the theme algorithm.
export const PROTOCOL_COLORS: Record<Protocol, string> = {
  openai: 'green',
  responses: 'purple',
  anthropic: 'orange',
};

// Canonical display order everywhere protocol lists are shown.
export const PROTOCOLS = ['openai', 'responses', 'anthropic'] as const;

// Sort key for display order; unknown protocol values sort last.
export const protocolOrder = (p: string): number => {
  const i = (PROTOCOLS as readonly string[]).indexOf(p);
  return i === -1 ? PROTOCOLS.length : i;
};

// The one protocol tag. Unknown protocol strings (RequestLog protocol_in/out
// are loosely typed) degrade to an uncolored tag. `dead` renders the
// struck-through variant used for protocols with no live channel.
export function ProtocolTag({
  protocol,
  dead = false,
  children,
  ...rest
}: { protocol: string; dead?: boolean } & TagProps) {
  return (
    <Tag
      {...rest}
      color={dead ? undefined : PROTOCOL_COLORS[protocol as Protocol]}
      style={{
        ...(dead ? { textDecoration: 'line-through', opacity: 0.6 } : undefined),
        ...rest.style,
      }}
    >
      {children ?? protocol}
    </Tag>
  );
}
