import { FlowLink, LinkLabel } from '@/components/flow/FlowLink';
import { edgeLabel } from '@/components/flow/labels';
import { useElementWidth } from '@/components/flow/useLiveStatus';
import type { MapLink, MapNode } from './clusterModel';
import { linkGeometry, placeCards } from './layout';
import { NodeTile } from './NodeTile';

export function ClusterMap({ nodes, links, reduced }: { nodes: MapNode[]; links: MapLink[]; reduced: boolean }) {
  const [ref, width] = useElementWidth<HTMLDivElement>();
  const place = placeCards(nodes.length, width);
  const at = new Map(nodes.map((n, i) => [n.name, place.centers[i]]));
  const center = { x: width / 2, y: place.height / 2 };

  const drawn = links
    .filter((l) => at.has(l.a) && at.has(l.b))
    .map((l) => ({ link: l, geo: linkGeometry(at.get(l.a)!, at.get(l.b)!, center) }));

  return (
    <div ref={ref} className="relative w-full" style={{ height: width ? place.height : 320 }}>
      {width > 0 && (
        <>
          <svg className="absolute inset-0" width={width} height={place.height} aria-hidden>
            {drawn.map(({ link, geo }) => (
              <FlowLink
                key={link.key}
                d={geo.fwd}
                kind={link.kind}
                state={link.state}
                reduced={reduced}
                width={2}
                dot={4}
                runs={link.flows.map((f) => ({
                  d: f.from === link.a ? geo.fwd : geo.back,
                  kind: f.kind,
                  level: f.level,
                }))}
              />
            ))}
          </svg>
          {nodes.map((n) => {
            const c = at.get(n.name)!;
            return (
              <div
                key={n.name}
                className="absolute"
                style={{ left: c.x - place.cardW / 2, top: c.y - place.cardH / 2 }}
              >
                <NodeTile node={n} width={place.cardW} height={place.cardH} />
              </div>
            );
          })}
          {/* Labels over the cards: a label is never hidden by the card it
              sits beside. */}
          <svg className="pointer-events-none absolute inset-0" width={width} height={place.height}>
            {drawn.map(({ link, geo }) => {
              const l = edgeLabel(link);
              if (!l) return null;
              const text = l.sub ? `${l.text} · ${l.sub}` : l.text;
              return (
                <LinkLabel key={link.key} x={geo.mid.x} y={geo.mid.y} tone={l.tone} size={11.5} text={text} />
              );
            })}
          </svg>
        </>
      )}
    </div>
  );
}
