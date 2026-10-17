import { useEffect, useRef, useState } from 'react';

// Shared pieces of the dashboard's charts. They are drawn at the pixel size
// they are shown at (useWidth), not stretched from a viewBox, so axis text
// keeps its size and lines keep their weight at any card width.

/** The width of the element the ref is attached to, kept current. */
export function useWidth<T extends HTMLElement>(): [React.RefObject<T | null>, number] {
  const ref = useRef<T>(null);
  const [width, setWidth] = useState(0);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    setWidth(el.clientWidth);
    const ro = new ResizeObserver(([e]) => setWidth(Math.floor(e.contentRect.width)));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return [ref, width];
}

export type Pt = [number, number];

/** A smooth path through the points, flattened so it never overshoots. */
export function smoothPath(pts: Pt[]): string {
  if (pts.length === 0) return '';
  let d = `M${pts[0][0].toFixed(1)},${pts[0][1].toFixed(1)}`;
  for (let i = 1; i < pts.length; i++) {
    const [px, py] = pts[i - 1];
    const [qx, qy] = pts[i];
    const mx = (px + qx) / 2;
    d += ` C${mx.toFixed(1)},${py.toFixed(1)} ${mx.toFixed(1)},${qy.toFixed(1)} ${qx.toFixed(1)},${qy.toFixed(1)}`;
  }
  return d;
}

/** A round upper bound for an axis, and three ticks under it. */
export function niceMax(v: number): number {
  if (!(v > 0)) return 1;
  const p = 10 ** Math.floor(Math.log10(v));
  for (const m of [1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) {
    if (m * p >= v) return m * p;
  }
  return 10 * p;
}

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];

/** Bytes in the largest unit that keeps the number above one. */
export function fmtBytes(v: number, digits = 1): string {
  let i = 0;
  let x = Math.abs(v);
  while (x >= 1000 && i < UNITS.length - 1) {
    x /= 1000;
    i++;
  }
  const s = x >= 100 || i === 0 ? x.toFixed(0) : x.toFixed(digits);
  return `${v < 0 ? '-' : ''}${s.replace(/\.0$/, '')} ${UNITS[i]}`;
}

/** Binary sizes for capacity, the way pools are sized. */
export function fmtSize(v: number): string {
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let i = 0;
  let x = v;
  while (x >= 1024 && i < u.length - 1) {
    x /= 1024;
    i++;
  }
  return `${x >= 100 || i === 0 ? x.toFixed(0) : x.toFixed(1).replace(/\.0$/, '')} ${u[i]}`;
}

export const fmtRate = (v: number) => `${fmtBytes(v)}/s`;

export const fmtPct = (v: number) => `${Math.round(v * 100)}%`;

export function fmtMs(seconds: number): string {
  const ms = seconds * 1000;
  return ms >= 10 ? `${ms.toFixed(0)} ms` : `${ms.toFixed(1).replace(/\.0$/, '')} ms`;
}

export function fmtCount(v: number): string {
  return v.toLocaleString('en-US');
}

/** A time axis label: clock time within two days, a date beyond. */
export function fmtTick(unix: number, span: number): string {
  const d = new Date(unix * 1000);
  if (span <= 2 * 86400) {
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
  }
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' });
}
