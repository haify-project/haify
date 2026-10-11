import { useEffect, useRef, useSyncExternalStore } from 'react';

// One requestAnimationFrame loop for every moving link on the page. Each link
// registers its dot path and a speed; the loop advances the dash offset. Speed
// is read from a ref every frame, so a new rate eases the dots along instead of
// restarting an animation.

interface Mover {
  el: SVGPathElement;
  speed: { current: number };
  period: { current: number };
  offset: number;
}

const movers = new Set<Mover>();
let frame = 0;
let last = 0;

function tick(now: number) {
  // A frame after a long pause (a background tab) must not jump.
  const dt = last ? Math.min(now - last, 100) / 1000 : 0;
  last = now;
  for (const m of movers) {
    const p = m.period.current;
    m.offset = (m.offset - m.speed.current * dt) % p;
    m.el.style.strokeDashoffset = String(m.offset);
  }
  frame = movers.size ? requestAnimationFrame(tick) : 0;
  if (!frame) last = 0;
}

/** Drives a dashed path along itself at `speed` px/s while it is mounted. */
export function useMovingDash(speed: number, period: number) {
  const ref = useRef<SVGPathElement>(null);
  const speedRef = useRef(speed);
  const periodRef = useRef(period);
  speedRef.current = speed;
  periodRef.current = period;
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const m: Mover = { el, speed: speedRef, period: periodRef, offset: Math.random() * -period };
    movers.add(m);
    if (!frame) frame = requestAnimationFrame(tick);
    return () => {
      movers.delete(m);
    };
    // The loop reads speed and period through refs; registering once is enough.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return ref;
}

const QUERY = '(prefers-reduced-motion: reduce)';

function subscribe(cb: () => void) {
  const mq = window.matchMedia?.(QUERY);
  mq?.addEventListener?.('change', cb);
  return () => mq?.removeEventListener?.('change', cb);
}

/** Whether the viewer asked the system for less motion. */
export function useReducedMotion(): boolean {
  return useSyncExternalStore(
    subscribe,
    () => Boolean(window.matchMedia?.(QUERY).matches),
    () => false,
  );
}

/** Intensity 0..1 → dot speed (px/s) and spacing (px). */
export function dotMotion(level: number): { speed: number; gap: number } {
  const l = Math.min(1, Math.max(0, level));
  // Spacing is stepped: a pattern that changed every poll would flicker.
  const gap = Math.round(30 - 18 * l);
  return { speed: 16 + 150 * l, gap: Math.max(10, gap) };
}
