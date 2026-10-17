import { Resource } from '../../services/api';
import { Button } from '@/components/ui/button';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import {
  ArrowUpCircle,
  ArrowDownCircle,
  Database,
  FolderCog,
  Trash2,
  MoreHorizontal,
  Camera,
  SlidersHorizontal,
  CalendarClock,
  Globe,
} from 'lucide-react';
import { type RowDialog } from './types';

// Every action a row can take, behind one control. A row of six buttons reads
// as six decisions to make; a `⋯` reads as one, and the destructive ones stay
// destructive inside it.
export function ResourceActionsMenu({
  resource,
  onSelect,
}: {
  resource: Resource;
  onSelect: (d: RowDialog) => void;
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          variant="ghost"
          size="icon"
          className="h-8 w-8 text-muted-foreground hover:text-foreground"
          aria-label={`Actions for ${resource.name}`}
        >
          <MoreHorizontal className="h-4 w-4" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-52">
        <DropdownMenuLabel className="font-mono">{resource.name}</DropdownMenuLabel>
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={() => onSelect('primary')}>
          <ArrowUpCircle className="h-4 w-4" />
          Set Primary
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onSelect('secondary')}>
          <ArrowDownCircle className="h-4 w-4" />
          Set Secondary
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onSelect('volumes')}>
          <Database className="h-4 w-4" />
          Volumes
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onSelect('snapshots')}>
          <Camera className="h-4 w-4" />
          Snapshots
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onSelect('mount')}>
          <FolderCog className="h-4 w-4" />
          Filesystem / Mount
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onSelect('options')}>
          <SlidersHorizontal className="h-4 w-4" />
          Edit DRBD options
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onSelect('schedule')}>
          <CalendarClock className="h-4 w-4" />
          Snapshot Schedule
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        {/* Off-site DR: attach one if there is none, fail over to it if there
            is. The two are mutually exclusive states of the same resource, so
            only one of them is ever offered. */}
        {resource.wanMode ? (
          <DropdownMenuItem variant="destructive" onSelect={() => onSelect('dr-failover')}>
            <Globe className="h-4 w-4" />
            DR Failover
          </DropdownMenuItem>
        ) : (
          <DropdownMenuItem onSelect={() => onSelect('add-dr')}>
            <Globe className="h-4 w-4" />
            Add DR Site
          </DropdownMenuItem>
        )}
        <DropdownMenuSeparator />
        <DropdownMenuItem variant="destructive" onSelect={() => onSelect('delete')}>
          <Trash2 className="h-4 w-4" />
          Delete
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
