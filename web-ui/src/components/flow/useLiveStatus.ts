import { useEffect, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '@/services/api';
import { useStatusRates } from './rates';

/** How often a resource on screen is re-read for its traffic counters. */
export const LIVE_POLL_MS = 3000;

/** Marks a query whose refetches are too frequent to announce in the header. */
export const LIVE_META = { live: true } as const;

/** Whether the element is on screen (or nearly). A callback ref. */
export function useOnScreen<T extends Element>() {
  const [el, setEl] = useState<T | null>(null);
  const [on, setOn] = useState(false);
  useEffect(() => {
    if (!el) {
      setOn(false);
      return;
    }
    const io = new IntersectionObserver(([e]) => setOn(e.isIntersecting), { rootMargin: '150px 0px' });
    io.observe(el);
    return () => io.disconnect();
  }, [el]);
  return [setEl, on] as const;
}

/** The element's content width in px, kept current as it resizes. */
export function useElementWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null);
  const [width, setWidth] = useState(0);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver(([e]) => setWidth(Math.floor(e.contentRect.width)));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return [ref, width] as const;
}

/**
 * A resource's status, re-read every few seconds while `active` (its view is on
 * screen; react-query already pauses intervals in a hidden tab), with the
 * traffic rates between consecutive readings.
 */
export function useLiveStatus(name: string, queryKey: readonly unknown[], active: boolean) {
  const q = useQuery({
    queryKey,
    queryFn: () => api.resourceStatus(name),
    refetchInterval: active ? LIVE_POLL_MS : false,
    meta: LIVE_META,
  });
  const status = q.data?.status;
  const rates = useStatusRates(JSON.stringify(queryKey), status, q.dataUpdatedAt);
  return { status, rates };
}
