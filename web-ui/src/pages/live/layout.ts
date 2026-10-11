// Where the node cards go on the live map, for any width from a phone up.
//
// Nodes sit on an ellipse: a ring reads as "these replicate to each other",
// and with two to eight nodes every link stays visible. The ring grows taller
// until no two cards overlap. A phone is too narrow for a ring, so there the
// cards zigzag down, one per row, and the links cross the gaps between rows.

export interface Placement {
  cardW: number;
  cardH: number;
  height: number;
  /** Card centres. */
  centers: { x: number; y: number }[];
}

const GAP = 18;
const PAD = 6;
const MAX_RING_H = 900;

function cardSize(width: number) {
  if (width >= 900) return { cardW: 212, cardH: 122 };
  if (width >= 600) return { cardW: 188, cardH: 122 };
  // Zigzag rows never share a height, so a phone's cards can be wide.
  return { cardW: Math.round(Math.min(220, Math.max(150, width * 0.62))), cardH: 122 };
}

function overlaps(c: { x: number; y: number }[], w: number, h: number) {
  for (let i = 0; i < c.length; i++)
    for (let j = i + 1; j < c.length; j++)
      if (Math.abs(c[i].x - c[j].x) < w + GAP && Math.abs(c[i].y - c[j].y) < h + GAP) return true;
  return false;
}

export function placeCards(n: number, width: number): Placement {
  const { cardW, cardH } = cardSize(width);
  if (n === 0) return { cardW, cardH, height: 160, centers: [] };
  if (n === 1) return { cardW, cardH, height: cardH + 2 * PAD, centers: [{ x: width / 2, y: cardH / 2 + PAD }] };

  if (width < 600) return zigzag(n, width, cardW, cardH);

  // Even counts start half a step round, so four nodes make a square and six a
  // hexagon with flat top and bottom.
  const start = -Math.PI / 2 - (n % 2 === 0 ? Math.PI / n : 0);
  const rx = Math.max(0, width / 2 - cardW / 2 - PAD);
  const ring = (height: number) => {
    const ry = Math.max(0, height / 2 - cardH / 2 - PAD);
    return Array.from({ length: n }, (_, i) => {
      const a = start + (i * 2 * Math.PI) / n;
      return { x: width / 2 + rx * Math.cos(a), y: height / 2 + ry * Math.sin(a) };
    });
  };

  // Two nodes on a wide screen read best side by side at a modest height.
  let height = Math.min(Math.max(cardH * 2 + 140, width * 0.42), 600);
  if (n === 2) height = cardH + 2 * PAD;
  let centers = ring(height);
  while (overlaps(centers, cardW, cardH) && height < MAX_RING_H) {
    height += 16;
    centers = ring(height);
  }
  // Cards side by side at the same height never part by growing taller.
  if (overlaps(centers, cardW, cardH)) return zigzag(n, width, cardW, cardH);
  return { cardW, cardH, height: Math.ceil(height), centers };
}

/** Two columns, alternating, with a gap between rows for the links to cross. */
function zigzag(n: number, width: number, cardW: number, cardH: number): Placement {
  const step = cardH + 34;
  const left = PAD + cardW / 2;
  const right = width - PAD - cardW / 2;
  return {
    cardW,
    cardH,
    height: Math.ceil(cardH + 2 * PAD + (n - 1) * step),
    centers: Array.from({ length: n }, (_, i) => ({
      x: i % 2 === 0 ? left : right,
      y: PAD + cardH / 2 + i * step,
    })),
  };
}

/**
 * A link between two card centres: a gentle curve, bent toward the middle of
 * the ring so chords between different pairs separate instead of overlapping.
 */
export function linkGeometry(
  a: { x: number; y: number },
  b: { x: number; y: number },
  center: { x: number; y: number },
) {
  const mx = (a.x + b.x) / 2;
  const my = (a.y + b.y) / 2;
  const dx = b.x - a.x;
  const dy = b.y - a.y;
  const len = Math.hypot(dx, dy) || 1;
  let nx = -dy / len;
  let ny = dx / len;
  const toCenter = (center.x - mx) * nx + (center.y - my) * ny;
  if (Math.hypot(center.x - mx, center.y - my) > 12 && toCenter < 0) {
    nx = -nx;
    ny = -ny;
  }
  const k = Math.min(70, len * 0.14);
  const cx = mx + nx * k;
  const cy = my + ny * k;
  return {
    fwd: `M ${a.x} ${a.y} Q ${cx} ${cy} ${b.x} ${b.y}`,
    back: `M ${b.x} ${b.y} Q ${cx} ${cy} ${a.x} ${a.y}`,
    // The curve's midpoint.
    mid: { x: (a.x + 2 * cx + b.x) / 4, y: (a.y + 2 * cy + b.y) / 4 },
  };
}
