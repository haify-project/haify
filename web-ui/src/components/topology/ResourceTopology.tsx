import type { Resource, ResourceStatus } from '@/services/api';
import { FlowLink, LinkLabel, type FlowRun } from '@/components/flow/FlowLink';
import { type FlowEdge, type LinkState, type ResourceFlows } from '@/components/flow/model';
import { edgeLabel } from '@/components/flow/labels';
import { Legend, NODE_H, NODE_W, NodeCard, SiteFrame } from './NodeCard';
import { placeNodes } from './placement';

// Where a resource's copies live, how they are wired, and what is moving
// between them right now.
//
// The primary site is a stack of nodes; its links are arcs in the gutter on
// the stack's left, one per pair the status describes, each carrying dots in
// the direction data travels. The DR site, when there is one, sits to the
// right with a dashed WAN leg from every primary-site replica.
//
// The component draws the diagram and nothing else: no border, no padding, no
// heading. The card it sits in owns those.

const NODE_GAP = 14;
const SITE_PAD = 22;
const SITE_HEADER = 34;
const COL_GAP = 168;
const MARGIN = 12;
// Arc bulge: wide enough that a label centred on the apex clears the cards.
const ARC_MIN = 56;
const ARC_STEP = 24;
const LABEL_HALF = 40;
// Stacked layout: room on the right for the WAN legs, and between the sites.
const LEG_GUTTER = 56;
const STACK_GAP = 64;

// SVG text scales with the viewBox; 1.6× of the natural geometry is as far as
// it may grow before the labels stop reading as labels.
const MAX_UPSCALE = 1.6;

/** Dot paths for an edge, each drawn in the direction its data travels. */
function runsOf(edge: FlowEdge | undefined, fwd: string, back: string, a: string): FlowRun[] {
  if (!edge) return [];
  return edge.flows.map((f) => ({ d: f.from === a ? fwd : back, kind: f.kind, level: f.level }));
}

export function ResourceTopology({
  resource,
  status,
  flows,
  reduced,
  stacked = false,
}: {
  resource: Resource;
  status: ResourceStatus;
  flows: ResourceFlows;
  reduced: boolean;
  /** Put the DR site under the primary site, for a narrow container. */
  stacked?: boolean;
}) {
  const { local, remote, backing, legState } = placeNodes(resource, status);
  const index = new Map(local.map((n, i) => [n.name, i]));
  const drName = remote[0]?.name;

  const localEdges = flows.edges.filter((e) => index.has(e.a) && index.has(e.b));
  const bulgeOf = (e: FlowEdge) =>
    ARC_MIN + ARC_STEP * (Math.abs(index.get(e.a)! - index.get(e.b)!) - 1) + (e.a === flows.primary ? 0 : 12);
  const maxBulge = localEdges.reduce((m, e) => Math.max(m, bulgeOf(e)), 0);
  const gutter = localEdges.length ? Math.ceil(maxBulge * 0.75 + LABEL_HALF + 6) : 0;

  const siteH = (count: number) =>
    SITE_HEADER + SITE_PAD * 2 + count * NODE_H + Math.max(0, count - 1) * NODE_GAP;
  const localW = SITE_PAD * 2 + gutter + NODE_W;
  const remoteW = SITE_PAD * 2 + NODE_W;
  const localH = siteH(local.length);
  const remoteH = remote.length > 0 ? siteH(remote.length) : 0;
  const vertical = stacked && remote.length > 0;
  const bodyH = Math.max(localH, remoteH);
  const width = vertical
    ? MARGIN * 2 + localW + LEG_GUTTER
    : MARGIN * 2 + localW + (remote.length > 0 ? COL_GAP + remoteW : 0);
  const height = vertical ? MARGIN * 2 + localH + STACK_GAP + remoteH : MARGIN * 2 + bodyH;

  const localX = MARGIN;
  const cardX = localX + SITE_PAD + gutter;
  // Side by side, the shorter site is centred against the taller one so the
  // WAN legs read as horizontal links rather than a lopsided fan. Stacked, the
  // DR card sits straight under the primary site's cards.
  const remoteX = vertical ? cardX - SITE_PAD : MARGIN + localW + COL_GAP;
  const localY = vertical ? MARGIN : MARGIN + (bodyH - localH) / 2;
  const remoteY = vertical ? MARGIN + localH + STACK_GAP : MARGIN + (bodyH - remoteH) / 2;
  const nodeY = (siteTop: number, i: number) => siteTop + SITE_HEADER + SITE_PAD + i * (NODE_H + NODE_GAP);

  const writeRateOf = (name: string) =>
    name === flows.primary && flows.writeRate !== undefined ? flows.writeRate : undefined;

  const pair = (a: string, b: string) =>
    flows.edges.find((e) => (e.a === a && e.b === b) || (e.a === b && e.b === a));

  return (
    <div>
      <Legend
        protocol={resource.protocol}
        hasWAN={remote.length > 0}
        hasTiebreaker={local.some((n) => n.kind === 'tiebreaker')}
        hasClient={local.some((n) => n.kind === 'client')}
      />
      <div className="mx-auto w-full" style={{ maxWidth: width * MAX_UPSCALE }}>
        <svg
          viewBox={`0 0 ${width} ${height}`}
          className="w-full"
          role="img"
          aria-label={`Replication topology for ${resource.name}`}
        >
          <SiteFrame
            x={localX}
            y={localY}
            w={localW}
            h={localH}
            label={remote.length > 0 ? 'Primary site' : 'Cluster'}
            sub={`tcp ${resource.port} · protocol ${resource.protocol}`}
          />
          {remote.length > 0 && (
            <SiteFrame
              x={remoteX}
              y={remoteY}
              w={remoteW}
              h={remoteH}
              label="DR site"
              sub={status.drEndpoint}
              dashed
            />
          )}

          {localEdges.map((e) => {
            const ya = nodeY(localY, index.get(e.a)!) + NODE_H / 2;
            const yb = nodeY(localY, index.get(e.b)!) + NODE_H / 2;
            const b = bulgeOf(e);
            const fwd = `M ${cardX} ${ya} C ${cardX - b} ${ya}, ${cardX - b} ${yb}, ${cardX} ${yb}`;
            const back = `M ${cardX} ${yb} C ${cardX - b} ${yb}, ${cardX - b} ${ya}, ${cardX} ${ya}`;
            return (
              <FlowLink
                key={e.key}
                d={fwd}
                kind={e.kind}
                state={e.state}
                runs={runsOf(e, fwd, back, e.a)}
                reduced={reduced}
                width={2}
              />
            );
          })}

          {/* One dashed WAN leg per primary-site replica. DRBD 9 is a full mesh,
              so whichever replica is Primary after a local failover needs its
              own path to the DR. */}
          {drName &&
            (() => {
              const legs = local.filter((m) => m.kind === 'replica');
              const words = legs.map((m) => {
                const st = legState(m.name);
                const line = st.replicating
                  ? status.wanPort
                    ? `tcp ${status.wanPort} · protocol A`
                    : 'protocol A'
                  : st.drState || 'not replicating';
                const caveat = st.replicating && st.proxy === 'inactive' ? 'proxy probe: inactive' : '';
                return { st, line, caveat, key: `${line}|${caveat}` };
              });
              const uniform = words.every((l) => l.key === words[0].key);
              const spokesman = legs[Math.floor((legs.length - 1) / 2)]?.name;
              return legs.map((n, li) => {
                const w = words[li];
                const edge = pair(n.name, drName);
                const state: LinkState =
                  edge && edge.state !== 'unknown' ? edge.state : w.st.replicating ? 'idle' : 'down';
                const x1 = cardX + NODE_W;
                const y1 = nodeY(localY, index.get(n.name)!) + NODE_H / 2;
                const x2 = vertical ? cardX + NODE_W : remoteX + SITE_PAD;
                const y2 = nodeY(remoteY, 0) + NODE_H / 2;
                // Stacked, every leg bows out to the right and back in.
                const bow = 30 + 7 * li;
                const [c1, c2] = vertical
                  ? [{ x: x1 + bow, y: y1 }, { x: x2 + bow, y: y2 }]
                  : [{ x: (x1 + x2) / 2, y: y1 }, { x: (x1 + x2) / 2, y: y2 }];
                const fwd = `M ${x1} ${y1} C ${c1.x} ${c1.y}, ${c2.x} ${c2.y}, ${x2} ${y2}`;
                const back = `M ${x2} ${y2} C ${c2.x} ${c2.y}, ${c1.x} ${c1.y}, ${x1} ${y1}`;
                const at = (t: number) => {
                  const u = 1 - t;
                  const [a, b, c, d] = [u * u * u, 3 * u * u * t, 3 * u * t * t, t * t * t];
                  return { x: a * x1 + b * c1.x + c * c2.x + d * x2, y: a * y1 + b * c1.y + c * c2.y + d * y2 };
                };
                // Side by side, labels slide along their own curve so converging
                // legs do not stack them on one point; stacked, they sit in the
                // gap between the sites.
                const gapY = remoteY - STACK_GAP / 2;
                const spread = uniform ? 0 : (li - (legs.length - 1) / 2) * 30;
                const p = vertical
                  ? { x: cardX + NODE_W / 2, y: gapY + spread }
                  : at(uniform || legs.length < 2 ? 0.5 : 0.3 + (0.4 * li) / (legs.length - 1));
                const showWords = !uniform || n.name === spokesman;
                const live = edgeLabel(edge);
                const q = vertical ? { x: cardX + NODE_W / 2, y: gapY + 8 } : at(0.5);
                const tone = state === 'down' ? 'bad' : 'muted';
                return (
                  <g key={`leg-${n.name}`}>
                    <FlowLink
                      d={fwd}
                      kind="wan"
                      state={state}
                      runs={runsOf(edge, n.name === edge?.a ? fwd : back, n.name === edge?.a ? back : fwd, edge?.a ?? '')}
                      reduced={reduced}
                      width={2}
                    />
                    {showWords && (
                      <LinkLabel
                        x={p.x}
                        y={p.y - 9}
                        tone={tone}
                        text={`${w.line}${uniform && legs.length > 1 ? ` · ${legs.length} legs` : ''}`}
                      />
                    )}
                    {showWords && w.caveat && (
                      <LinkLabel x={p.x} y={p.y + 7} tone="warn" text={w.caveat} />
                    )}
                    {live && live.tone !== 'bad' && (
                      <LinkLabel
                        x={q.x}
                        y={q.y + (w.caveat ? 23 : 9)}
                        tone={live.tone}
                        text={live.sub ? `${live.text} · ${live.sub}` : live.text}
                      />
                    )}
                  </g>
                );
              });
            })()}

          {/* Arc labels after every line, so no line crosses a label. */}
          {localEdges.map((e) => {
            const l = edgeLabel(e);
            if (!l) return null;
            const ya = nodeY(localY, index.get(e.a)!) + NODE_H / 2;
            const yb = nodeY(localY, index.get(e.b)!) + NODE_H / 2;
            const x = cardX - bulgeOf(e) * 0.75;
            const y = (ya + yb) / 2;
            return (
              <g key={`label-${e.key}`}>
                <LinkLabel x={x} y={l.sub ? y - 8 : y} tone={l.tone} halo="var(--muted)" text={l.text} />
                {l.sub && <LinkLabel x={x} y={y + 8} tone={l.tone} halo="var(--muted)" text={l.sub} />}
              </g>
            );
          })}

          {local.map((n, i) => (
            <NodeCard
              key={n.name}
              x={cardX}
              y={nodeY(localY, i)}
              node={n}
              backing={backing}
              writeRate={writeRateOf(n.name)}
            />
          ))}
          {remote.map((n, i) => (
            <NodeCard key={n.name} x={remoteX + SITE_PAD} y={nodeY(remoteY, i)} node={n} backing={backing} />
          ))}
        </svg>
      </div>
    </div>
  );
}
