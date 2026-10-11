import { measuredRate, type FlowEdge } from './model';
import { formatRate } from './rates';

export type LabelTone = 'muted' | 'primary' | 'warn' | 'bad' | 'ok';

/** What a link says in words, if anything: a rate, a resync, or why it is down. */
export function edgeLabel(
  edge: Pick<FlowEdge, 'state' | 'flows' | 'syncPercent' | 'word'> | undefined,
): { text: string; sub?: string; tone: LabelTone } | undefined {
  if (!edge) return undefined;
  const rate = measuredRate(edge.flows);
  const pct = edge.syncPercent !== undefined ? `${Math.floor(edge.syncPercent)}%` : '';
  if (edge.state === 'down') return { text: edge.word ?? 'disconnected', tone: 'bad' };
  if (edge.state === 'paused') return { text: `paused ${pct}`.trim(), tone: 'warn' };
  if (edge.state === 'resync')
    return {
      text: `resync ${pct}`.trim(),
      sub: rate !== undefined ? formatRate(rate) : undefined,
      tone: 'warn',
    };
  if (rate !== undefined && edge.state === 'busy') return { text: formatRate(rate), tone: 'primary' };
  return undefined;
}
