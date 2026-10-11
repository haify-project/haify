import { TopologyView } from '@/components/TopologyView';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { type PromoterView } from './promoter';
import { QuorumPill } from './QuorumPill';

// ==================== Replication topology ====================

/**
 * The page's subject, and the first thing on it. This reuses
 * `ResourceTopology` rather than drawing a second renderer: it already places
 * the sites, the synchronous mesh and the dashed WAN legs from this same status
 * payload, and it carries its own legend. What the card adds is the connection
 * facts the SVG has no room for — the DRBD port, the protocol and the VIP.
 */
export function TopologySection({ promoters }: { promoters: PromoterView[] }) {
  return (
    <section className="space-y-4">
      <h2 className="text-[14.5px] font-semibold">Replication topology</h2>
      <div className="space-y-5">
        {promoters.map(({ config, resource, status }) => (
          <Card key={config.resource} className="gap-4 py-5">
            <CardHeader className="gap-1 px-5">
              <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
                <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
                  <CardTitle className="font-mono text-[15px]">
                    {config.resource}
                  </CardTitle>
                  {config.vip ? (
                    <span className="font-mono text-xs tabular-nums text-muted-foreground">
                      vip {config.vip}
                    </span>
                  ) : null}
                  {config.mountPoint ? (
                    <span className="font-mono text-xs text-muted-foreground">
                      {config.mountPoint}
                      {config.fsType ? ` · ${config.fsType}` : ''}
                    </span>
                  ) : null}
                </div>
                <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
                  {resource ? (
                    <span className="font-mono text-xs tabular-nums text-muted-foreground">
                      tcp {resource.port} · protocol {resource.protocol}
                    </span>
                  ) : null}
                  {status?.drEndpoint ? (
                    <span className="font-mono text-xs tabular-nums text-muted-foreground">
                      dr {status.drEndpoint}
                    </span>
                  ) : null}
                  <QuorumPill quorum={status?.quorum} />
                </div>
              </div>
            </CardHeader>
            {/* The viewBox'd SVG fills the width it is given. This is the box
                it would scroll in the day it stops; the page itself must never
                scroll sideways. */}
            <CardContent className="overflow-x-auto px-5">
              {resource && status ? (
                <TopologyView
                  resource={resource}
                  status={status}
                  statusKey={['ha-status', config.resource]}
                />
              ) : (
                <p className="text-[13px] text-muted-foreground">
                  Replication state for{' '}
                  <span className="font-mono">{config.resource}</span> has not
                  arrived yet — the resource is not in the cluster resource list,
                  or its status call has not returned.
                </p>
              )}
            </CardContent>
          </Card>
        ))}
      </div>
    </section>
  );
}
