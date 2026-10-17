import type { ResourceOption } from '@/services/api';

// What the options dialog edits and what it sends back. The controller takes
// a map of changes: a key with a value sets it, a key with an empty value
// removes it. Keys the dialog did not touch are not sent, so a shared-secret
// that is only shown as set is never written back.

export interface OptionRow {
  key: string;
  value: string;
  /** The key this row was loaded with; empty for a row the user added. */
  loaded: string;
  /** Loaded as a secret: an empty value means "unchanged". */
  secret: boolean;
}

/** Set by their own commands, so the dialog shows them but does not edit them. */
export const MANAGED_OPTIONS: Record<string, string> = {
  'options/auto-promote': 'Haify promotes resources itself, so this stays off',
  'net/allow-two-primaries': 'Set by haify resource dual-primary, only for as long as needed',
  'net/tls': 'Set by haify resource tls',
};

export function rowsFrom(options: ResourceOption[]): OptionRow[] {
  return options.map((o) => ({ key: o.key, value: o.value ?? '', loaded: o.key, secret: Boolean(o.secret) }));
}

export function optionChanges(loaded: ResourceOption[], rows: OptionRow[]): Record<string, string> {
  const before = new Map(loaded.map((o) => [o.key, o.value ?? '']));
  const changes: Record<string, string> = {};
  const kept = new Set<string>();
  for (const r of rows) {
    const key = r.key.trim();
    const value = r.value.trim();
    if (r.loaded && key === r.loaded) {
      kept.add(key);
      if (r.secret && value === '') continue;
      if (value !== before.get(key)) changes[key] = value;
      continue;
    }
    // A renamed or added row.
    if (key && value) changes[key] = value;
  }
  for (const o of loaded) if (!kept.has(o.key) && !(o.key in changes)) changes[o.key] = '';
  return changes;
}
