import { useEffect, useSyncExternalStore } from 'react';

// What the user is looking at, in a few words, for the Copilot: the layout
// names the page, and a page with filters can say more ("Resources,
// degraded") for as long as it is mounted. The Copilot shows it and sends it
// with each question.

let base = '';
let detail = '';
let current = '';
const listeners = new Set<() => void>();

function publish() {
  const next = detail || base;
  if (next === current) return;
  current = next;
  listeners.forEach((l) => l());
}

export function usePageContext(): string {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => current,
  );
}

/** The layout's part: the name of the page on screen. */
export function useNamePage(name: string) {
  useEffect(() => {
    base = name;
    publish();
  }, [name]);
}

/** A page's part: what it shows right now, more precisely than its name. */
export function useDescribePage(label: string | undefined) {
  useEffect(() => {
    detail = label ?? '';
    publish();
    return () => {
      detail = '';
      publish();
    };
  }, [label]);
}
