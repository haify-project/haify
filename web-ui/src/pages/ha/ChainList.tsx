import {
  Boxes,
  Cog,
  Database,
  Disc3,
  FolderOpen,
  Globe,
  HardDrive,
  Monitor,
  Share2,
  Zap,
  type LucideIcon,
} from 'lucide-react';
import { TONE_BG, toneOf } from '@/components/status';
import { cn } from '@/lib/utils';
import type { HaUnit } from '@/services/api';
import { unitKind } from './promoter';

// The promoter's start list as the services it brings up, in order.

const ICON: Record<string, LucideIcon> = {
  mount: HardDrive,
  service_ip: Globe,
  nfs: Share2,
  iscsi: Disc3,
  nvmeof: Zap,
  smb: FolderOpen,
  database: Database,
  vm: Monitor,
  haify: Boxes,
  service: Cog,
};

const KIND_NAME: Record<string, string> = {
  mount: 'mount',
  service_ip: 'service IP',
  nfs: 'NFS',
  iscsi: 'iSCSI',
  nvmeof: 'NVMe-oF',
  smb: 'SMB',
  database: 'database',
  vm: 'VM',
  haify: 'Haify',
  service: 'service',
};

/** "up 3h" from an RFC 3339 time; empty when there is none. */
export function upFor(since?: string, now = Date.now()): string {
  if (!since) return '';
  const t = new Date(since).getTime();
  if (Number.isNaN(t)) return '';
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 60) return `up ${s}s`;
  if (s < 3600) return `up ${Math.floor(s / 60)}m`;
  if (s < 86400) return `up ${Math.floor(s / 3600)}h`;
  return `up ${Math.floor(s / 86400)}d`;
}

type ChainUnit = Omit<HaUnit, 'status'> & { status?: string };

function ChainItem({ unit }: { unit: ChainUnit }) {
  const kind = unitKind(unit);
  const Icon = ICON[kind] ?? Cog;
  const label = unit.label || unit.name;
  const up = upFor(unit.since);
  const restarts = unit.restarts ?? 0;
  return (
    <li className="flex items-start gap-2.5">
      <span
        title={KIND_NAME[kind] ?? kind}
        className="mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-[5px] bg-muted text-muted-foreground"
      >
        <Icon className="size-3.5" aria-label={KIND_NAME[kind] ?? kind} />
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex min-w-0 items-baseline gap-2">
          {unit.status ? (
            <span
              aria-hidden
              title={unit.status}
              className={cn('size-[7px] shrink-0 -translate-y-px rounded-full', TONE_BG[toneOf(unit.status)])}
            />
          ) : null}
          <span
            className={cn(
              'min-w-0 break-words md:truncate',
              unit.label ? 'text-[13px] font-medium' : 'font-mono text-[12px] break-all',
            )}
            title={label}
          >
            {label}
          </span>
        </div>
        {label !== unit.name ? (
          <div className="truncate font-mono text-[11px] text-muted-foreground" title={unit.name}>
            {unit.name}
          </div>
        ) : null}
      </div>
      {up || restarts > 0 ? (
        <div className="flex shrink-0 flex-col items-end text-[11px] leading-[1.45] tabular-nums">
          {up ? (
            <span className="text-muted-foreground" title={unit.since}>
              {up}
            </span>
          ) : null}
          {restarts > 0 ? (
            <span className="text-status-warn-text">
              {restarts} restart{restarts === 1 ? '' : 's'}
            </span>
          ) : null}
        </div>
      ) : null}
    </li>
  );
}

export function ChainList({ units }: { units: ChainUnit[] }) {
  return (
    <ol className="mt-2 space-y-2">
      {units.map((u) => (
        <ChainItem key={u.name} unit={u} />
      ))}
    </ol>
  );
}
