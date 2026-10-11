import type { Resource, ResourceStatus } from '@/services/api';
import { resourceFlows } from './flow/model';
import { useReducedMotion } from './flow/motion';
import { useElementWidth, useLiveStatus, useOnScreen } from './flow/useLiveStatus';
import { ResourceTopology } from './topology/ResourceTopology';

/**
 * A resource's replication topology with its live traffic. While the drawing
 * is on screen its status is re-read every few seconds under `statusKey` — the
 * key the page already reads it under, so the page and the drawing share one
 * answer.
 */
export function TopologyView({
  resource,
  status,
  statusKey = ['resource-status', resource.name],
}: {
  resource: Resource;
  status: ResourceStatus;
  statusKey?: readonly unknown[];
}) {
  const [ref, onScreen] = useOnScreen<HTMLDivElement>();
  const live = useLiveStatus(resource.name, statusKey, onScreen);
  const reduced = useReducedMotion();
  const current = live.status ?? status;
  const flows = resourceFlows(resource, current, live.rates);
  const [box, width] = useElementWidth<HTMLDivElement>();
  return (
    <div ref={ref}>
      <div ref={box}>
        <ResourceTopology
          resource={resource}
          status={current}
          flows={flows}
          reduced={reduced}
          stacked={width > 0 && width < STACK_BELOW}
        />
      </div>
    </div>
  );
}

/** Below this width the DR site goes under the primary site, not beside it. */
const STACK_BELOW = 600;
