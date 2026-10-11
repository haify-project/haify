import { useMemo } from 'react';
import type { NodeResourceState, ResourceStatus } from '@/services/api';

// Live rates from the cumulative KiB counters in a resource's status.
//
// The controller reports totals since the device (or the connection) came up;
// a rate is the difference between two polls over the time between them. A
// counter that went down was reset (a reconnect, a restart), so that sample
// has no rate rather than a negative one. Nothing here is estimated: a field
// the controller did not send stays undefined all the way to the screen.

/** KiB/s per counter for one entry of `status.nodeStates`. */
export interface EntryRates {
  written?: number;
  read?: number;
  sent?: number;
  received?: number;
}

export interface StatusRates {
  /** Keyed like `status.nodeStates`: by DRBD host name. */
  byHost: Record<string, EntryRates>;
  /** Whether this controller reports the counters at all. */
  available: boolean;
}

type Field = keyof EntryRates;

const FIELDS: { field: Field; key: keyof NodeResourceState }[] = [
  { field: 'written', key: 'writtenKib' },
  { field: 'read', key: 'readKib' },
  { field: 'sent', key: 'sentKib' },
  { field: 'received', key: 'receivedKib' },
];

/** A uint64 counter as JSON hands it over: a string, occasionally a number. */
function counter(v: unknown): number | undefined {
  if (v === undefined || v === null || v === '') return undefined;
  const n = Number(v);
  return Number.isFinite(n) && n >= 0 ? n : undefined;
}

/** The host whose entry is the answering node's own: it has no connection. */
export function reporterHost(status: ResourceStatus | undefined): string | undefined {
  const entries = Object.entries(status?.nodeStates ?? {});
  const hasConnection = entries.some(([, s]) => s.connection !== undefined);
  const own = entries.find(([, s]) =>
    hasConnection ? !s.connection : !s.replicationState,
  );
  return own?.[0];
}

interface Sample {
  at: number;
  reporter?: string;
  values: Record<string, Partial<Record<Field, number>>>;
  rates: StatusRates;
}

// One sample per query key. Module level, so a view that remounts (a row
// collapsed and expanded again) keeps its previous reading.
const samples = new Map<string, Sample>();

// Two readings further apart than this describe an average over a period the
// viewer was not watching; they say nothing about now.
const MAX_GAP_MS = 30_000;
const MIN_GAP_MS = 400;

const EMPTY: StatusRates = { byHost: {}, available: false };

export function computeRates(
  key: string,
  status: ResourceStatus | undefined,
  at: number,
): StatusRates {
  if (!status || !at) return EMPTY;
  const prev = samples.get(key);
  if (prev && prev.at === at) return prev.rates;
  // An older reading arriving late is not a newer sample.
  if (prev && at < prev.at) return prev.rates;

  const reporter = reporterHost(status);
  const values: Sample['values'] = {};
  let available = false;
  for (const [host, st] of Object.entries(status.nodeStates ?? {})) {
    const v: Partial<Record<Field, number>> = {};
    for (const { field, key: k } of FIELDS) {
      const c = counter(st[k]);
      if (c !== undefined) {
        v[field] = c;
        available = true;
      }
    }
    values[host] = v;
  }

  const byHost: Record<string, EntryRates> = {};
  const gap = prev ? at - prev.at : 0;
  // A different node answering means different counters entirely.
  const comparable =
    prev !== undefined && prev.reporter === reporter && gap >= MIN_GAP_MS && gap <= MAX_GAP_MS;
  if (comparable) {
    for (const [host, v] of Object.entries(values)) {
      const before = prev.values[host];
      if (!before) continue;
      const r: EntryRates = {};
      for (const { field } of FIELDS) {
        const now = v[field];
        const then = before[field];
        if (now === undefined || then === undefined || now < then) continue;
        r[field] = (now - then) / (gap / 1000);
      }
      byHost[host] = r;
    }
  }

  const rates: StatusRates = { byHost, available };
  samples.set(key, { at, reporter, values, rates });
  return rates;
}

/** Rates for one status query; `key` names the query the samples belong to. */
export function useStatusRates(
  key: string,
  status: ResourceStatus | undefined,
  at: number,
): StatusRates {
  return useMemo(() => computeRates(key, status, at), [key, status, at]);
}

/** Below this a link is quiet: DRBD's own keep-alives are not data. */
export const BUSY_KIBS = 0.5;

/** KiB/s → animation intensity 0..1, logarithmic from 1 KiB/s to 1 GiB/s. */
export function flowLevel(kibPerSec: number): number {
  if (!(kibPerSec > 0)) return 0;
  return Math.min(1, Math.max(0, Math.log10(Math.max(1, kibPerSec)) / 6));
}

/** KiB/s → "12.4 MB/s". Decimal units, as disk and network tools print them. */
export function formatRate(kibPerSec: number): string {
  const bytes = kibPerSec * 1024;
  const units = ['B/s', 'KB/s', 'MB/s', 'GB/s'];
  let v = bytes;
  let i = 0;
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000;
    i++;
  }
  const digits = v >= 100 || i === 0 ? 0 : 1;
  return `${v.toFixed(digits)} ${units[i]}`;
}
