import { useQuery } from '@tanstack/react-query';
import { api, type Pool } from '../services/api';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { Database, Server } from 'lucide-react';
import { PoolItem } from './pools/PoolItem';
import { CreatePoolDialog } from './pools/PoolDialogs';
import { TrimButton } from './pools/TrimButton';
import { StorageJobsPanel } from './pools/StorageJobsPanel';
import { DisksPanel } from './pools/DisksPanel';
import { UnusedDisksPanel } from './pools/UnusedDisksPanel';

export function PoolsPage() {
  const { data: pools, isLoading } = useQuery({
    queryKey: ['pools'],
    queryFn: () => api.getPools(),
  });

  const { data: nodes } = useQuery({
    queryKey: ['nodes'],
    queryFn: () => api.getNodes(),
  });

  const nodeMap =
    nodes?.nodes.reduce(
      (acc, node) => {
        acc[node.address] = node.name;
        return acc;
      },
      {} as Record<string, string>,
    ) ?? {};

  if (isLoading) {
    return (
      <div className="@container space-y-6">
        <div className="flex items-center justify-between">
          <h3 className="text-lg font-semibold">Storage Pools</h3>
        </div>
        <div className="grid grid-cols-1 gap-6 @min-[680px]:grid-cols-2 @min-[1080px]:grid-cols-3">
          {[0, 1, 2].map((i) => (
            <Card key={i}>
              <CardHeader>
                <Skeleton className="h-6 w-40" />
              </CardHeader>
              <CardContent className="space-y-3">
                <Skeleton className="h-16 w-full" />
                <Skeleton className="h-16 w-full" />
              </CardContent>
            </Card>
          ))}
        </div>
      </div>
    );
  }

  const poolsByNode =
    pools?.pools.reduce(
      (acc, pool) => {
        if (!acc[pool.node]) acc[pool.node] = [];
        acc[pool.node].push(pool);
        return acc;
      },
      {} as Record<string, Pool[]>,
    ) ?? {};

  const nodeEntries = Object.entries(poolsByNode);

  // The page is a container so the columns of node cards follow the width it
  // actually has, which the sidebar and a docked Copilot both take from.
  return (
    <div className="@container space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h3 className="text-lg font-semibold">Storage Pools</h3>
        <div className="flex items-center gap-2">
          <TrimButton />
          <CreatePoolDialog nodes={nodes?.nodes ?? []} />
        </div>
      </div>

      <StorageJobsPanel />

      <UnusedDisksPanel pools={pools?.pools ?? []} nodeNames={nodeMap} />

      {nodeEntries.length === 0 ? (
        <div className="flex flex-col items-center justify-center gap-3 py-16 text-muted-foreground">
          <Database className="h-10 w-10" />
          <p>No storage pools found. Create your first pool to get started.</p>
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-6 @min-[680px]:grid-cols-2 @min-[1080px]:grid-cols-3">
          {nodeEntries.map(([nodeAddr, nodePools]) => {
            const nodeName = nodeMap[nodeAddr] || nodeAddr;
            return (
              <Card key={nodeAddr}>
                <CardHeader>
                  <CardTitle className="flex items-center gap-3">
                    <span className="flex h-10 w-10 items-center justify-center rounded-full bg-primary/10">
                      <Server className="h-5 w-5 text-primary" />
                    </span>
                    <span className="flex flex-col">
                      <span>{nodeName}</span>
                      <span className="text-xs font-normal text-muted-foreground">
                        {nodeAddr} &bull; {nodePools.length} pool
                        {nodePools.length > 1 ? 's' : ''}
                      </span>
                    </span>
                  </CardTitle>
                </CardHeader>
                <CardContent className="space-y-3">
                  {nodePools.map((pool) => (
                    <PoolItem key={`${pool.node}-${pool.name}`} pool={pool} nodeName={nodeName} />
                  ))}
                </CardContent>
              </Card>
            );
          })}
        </div>
      )}

      {nodeEntries.length > 0 && <DisksPanel />}
    </div>
  );
}

