import { lazy, Suspense, useEffect, useRef, useState } from 'react';
import { Outlet, Link, useLocation, useNavigate } from 'react-router';
import { useIsFetching, useQuery } from '@tanstack/react-query';
import {
  LayoutDashboard,
  Server,
  Database,
  Box,
  Network,
  ShieldCheck,
  Lock,
  Loader2,
  Sparkles,
  Menu,
  PanelLeft,
  ScrollText,
  SlidersHorizontal,
  BellRing,
  Activity,
  Monitor,
  Cpu,
  Search,
  ChevronRight,
} from 'lucide-react';
import { AI_ENABLED, HAIFY_VERSION } from '@/features';
import { cn } from '@/lib/utils';
import { api } from '@/services/api';
import { isPveManaged } from '@/pages/proxmox/pve';
import { libvirtDomainOf } from '@/pages/guests/libvirt';
import { UserMenu } from '@/components/UserMenu';
import { NotificationBell } from '@/components/NotificationBell';
import { HaifyMark } from '@/components/HaifyMark';
import { useNamePage } from '@/lib/pageContext';

// Collapsing the sidebar is a per-workstation preference, so it is remembered
// across reloads. Storage can throw (private mode, disabled cookies); the
// preference is not worth failing a render over.
const COLLAPSE_KEY = 'haify.sidebar.collapsed';

function readCollapsed(): boolean {
  try {
    return localStorage.getItem(COLLAPSE_KEY) === '1';
  } catch {
    return false;
  }
}

function writeCollapsed(value: boolean) {
  try {
    localStorage.setItem(COLLAPSE_KEY, value ? '1' : '0');
  } catch {
    /* preference simply won't persist */
  }
}

// The Copilot pulls in the whole Markdown/streaming rendering stack — by far
// the heaviest thing in the bundle, and most sessions never open it. Load it
// on first use instead of making every page wait for it.
const AICopilot = AI_ENABLED
  ? lazy(() => import('@/components/AICopilot').then((m) => ({ default: m.AICopilot })))
  : null;

// PageFallback stands in while a lazily-loaded route chunk arrives. It fills
// the content area only: the sidebar and header are already rendered and must
// not flicker on navigation.
function PageFallback() {
  return (
    <div
      className="flex h-full min-h-64 items-center justify-center"
      role="status"
      aria-live="polite"
    >
      <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
      <span className="sr-only">Loading</span>
    </div>
  );
}

type NavItem = {
  name: string;
  href: string;
  icon: typeof LayoutDashboard;
  /** An integration's page, listed only once the cluster has its disks. */
  integration?: 'pve' | 'libvirt';
};

// Two groups, because the destinations answer two different questions:
// "what is the cluster made of" and "what is it doing / who may touch it".
// Nine flat rows made the operator read the whole list every time.
const navGroups: { label: string; items: NavItem[] }[] = [
  {
    // The cluster's own pages come first and need no heading.
    label: '',
    items: [
      { name: 'Dashboard', href: '/dashboard', icon: LayoutDashboard },
      { name: 'Live map', href: '/live', icon: Activity },
      { name: 'Nodes', href: '/nodes', icon: Server },
      { name: 'Pools', href: '/pools', icon: Database },
      { name: 'Resources', href: '/resources', icon: Box },
      { name: 'Gateways', href: '/gateways', icon: Network },
      { name: 'HA', href: '/ha', icon: ShieldCheck },
      { name: 'Proxmox VE', href: '/proxmox', icon: Monitor, integration: 'pve' },
      { name: 'KVM', href: '/kvm', icon: Cpu, integration: 'libvirt' },
    ],
  },
  {
    label: 'Operations',
    items: [
      { name: 'Notifications', href: '/notifications', icon: BellRing },
      { name: 'Access', href: '/access', icon: Lock },
      { name: 'Logs', href: '/logs', icon: ScrollText },
    ],
  },
];

// Settings holds only the Copilot's model, so it sits with the Copilot under
// Tools rather than among the cluster's pages.
const settingsItem: NavItem = { name: 'Settings', href: '/settings', icon: SlidersHorizontal };

// Flat view of the same data, for the current-section lookup below. Sorted
// deepest-path-first once at module load rather than on every render: the
// lookup wants the most specific match, and the input is a constant.
const navByDepth = [...navGroups.flatMap((g) => g.items), ...(AI_ENABLED ? [settingsItem] : [])]
  .sort((a, b) => b.href.length - a.href.length);

// SidebarContent is the shared nav body, rendered both in the static desktop
// sidebar and inside the mobile slide-in drawer. onNavigate lets the mobile
// drawer close itself when a link is tapped.
//
// `collapsed` is desktop-only: the mobile drawer always renders full width, so
// it never passes it.
function SidebarContent({
  pathname,
  onNavigate,
  collapsed = false,
  aiOpen,
  onToggleCopilot,
}: {
  pathname: string;
  onNavigate?: () => void;
  collapsed?: boolean;
  aiOpen: boolean;
  onToggleCopilot: () => void;
}) {
  // Same query key as the Resources page, so this costs no extra request.
  const { data: resources } = useQuery({ queryKey: ['resources'], queryFn: () => api.getResources() });
  const all = resources?.resources ?? [];
  const integrations = new Set<NavItem['integration']>([
    ...(all.some(isPveManaged) ? (['pve'] as const) : []),
    ...(all.some((r) => libvirtDomainOf(r)) ? (['libvirt'] as const) : []),
  ]);
  return (
    <div
      className={cn(
        'flex min-h-0 flex-1 flex-col py-5',
        collapsed ? 'px-2' : 'px-3',
      )}
    >
      <div
        className={cn(
          'flex items-center pb-5',
          collapsed ? 'justify-center' : 'gap-2.5 px-2.5',
        )}
      >
        <HaifyMark />
        {!collapsed && <span className="text-[15px] font-semibold tracking-tight">Haify</span>}
      </div>

      {!collapsed && <SidebarSearch onNavigate={onNavigate} />}

      {/* `overflow-y: auto` forces `overflow-x: auto` too, which would shave
          the active pill's 1px outward ring on both sides. The px/-mx pair
          gives the ring the pixel it needs without moving anything. */}
      <nav className="-mx-px flex-1 space-y-5 overflow-y-auto px-px">
        {navGroups.map((group) => (
          <div key={group.label}>
            {/* The label is the point of the grouping; collapsed there is no
                room for it and the gap between blocks carries it instead. */}
            {!collapsed && group.label && (
              <div className="eyebrow px-2.5 pb-2">{group.label}</div>
            )}
            <div className="flex flex-col gap-0.5">
              {group.items.filter((item) => !item.integration || integrations.has(item.integration)).map((item) => {
                const isActive =
                  pathname === item.href || pathname.startsWith(item.href + '/');
                return (
                  <Link
                    key={item.name}
                    to={item.href}
                    onClick={onNavigate}
                    // Collapsed leaves only an icon, so the name has to survive
                    // for screen readers and as a hover hint.
                    title={collapsed ? item.name : undefined}
                    aria-label={collapsed ? item.name : undefined}
                    className={cn(
                      'flex h-[34px] items-center gap-2.5 rounded-md text-[13.5px] transition-colors',
                      collapsed ? 'justify-center px-0' : 'px-2.5',
                      isActive
                        ? 'bg-sidebar-accent font-medium text-foreground'
                        : 'text-muted-foreground hover:bg-sidebar-accent/60 hover:text-foreground',
                    )}
                  >
                    <item.icon
                      className={cn(
                        'size-[17px] shrink-0',
                        isActive && 'text-primary',
                      )}
                    />
                    {!collapsed && item.name}
                  </Link>
                );
              })}
            </div>
          </div>
        ))}
        {AI_ENABLED && (
          <div>
            {!collapsed && <div className="eyebrow px-2.5 pb-2">Tools</div>}
            <div className="flex flex-col gap-0.5">
              <button
                type="button"
                onClick={onToggleCopilot}
                aria-pressed={aiOpen}
                aria-label="Copilot"
                title={collapsed ? 'Copilot' : undefined}
                className={cn(
                  'flex h-[34px] items-center gap-2.5 rounded-md text-[13.5px] transition-colors',
                  collapsed ? 'justify-center px-0' : 'px-2.5',
                  aiOpen
                    ? 'bg-sidebar-accent font-medium text-foreground'
                    : 'text-muted-foreground hover:bg-sidebar-accent/60 hover:text-foreground',
                )}
              >
                <Sparkles className={cn('size-[17px] shrink-0', aiOpen && 'text-primary')} />
                {!collapsed && (
                  <>
                    Ask Copilot
                    <kbd className="ml-auto rounded border border-sidebar-border px-1 font-sans text-[10.5px] text-muted-foreground">⌘J</kbd>
                  </>
                )}
              </button>
              <Link
                to={settingsItem.href}
                onClick={onNavigate}
                title={collapsed ? settingsItem.name : undefined}
                aria-label={collapsed ? settingsItem.name : undefined}
                className={cn(
                  'flex h-[34px] items-center gap-2.5 rounded-md text-[13.5px] transition-colors',
                  collapsed ? 'justify-center px-0' : 'px-2.5',
                  pathname.startsWith(settingsItem.href)
                    ? 'bg-sidebar-accent font-medium text-foreground'
                    : 'text-muted-foreground hover:bg-sidebar-accent/60 hover:text-foreground',
                )}
              >
                <SlidersHorizontal className="size-[17px] shrink-0" />
                {!collapsed && settingsItem.name}
              </Link>
            </div>
          </div>
        )}
      </nav>

      {/* No `mt-auto`: the sibling <nav> already carries `flex-1` in this
          flex-col and pushes the footer down. */}
      <div className="pt-4">
        {collapsed ? (
          <div className="text-center font-mono text-[10px] text-muted-foreground">{HAIFY_VERSION.split('-')[0]}</div>
        ) : (
          <ControlPlaneCard onNavigate={onNavigate} />
        )}
      </div>
    </div>
  );
}

// Where the controller runs, the one fact about the cluster worth keeping on
// every page, and the release this UI belongs to.
function ControlPlaneCard({ onNavigate }: { onNavigate?: () => void }) {
  const { data: ha } = useQuery({ queryKey: ['selfha'], queryFn: () => api.getSelfHaStatus() });
  const { data: nodes } = useQuery({ queryKey: ['nodes'], queryFn: () => api.getNodes() });
  const active = ha?.activeNode
    ? (nodes?.nodes.find((n) => n.address === ha.activeNode)?.name ?? ha.activeNode)
    : '';
  const line = !ha ? '' : !ha.enabled ? 'Standalone controller' : active ? `Control plane on ${active}` : 'Control plane moving';
  return (
    <Link
      to="/ha"
      onClick={onNavigate}
      className="flex items-center gap-2.5 rounded-xl border border-sidebar-border bg-card p-2.5 transition-colors hover:border-ring/40"
    >
      <HaifyMark className="size-7" />
      <span className="min-w-0 flex-1">
        <span className="block truncate text-[12.5px] font-semibold">{line || 'Haify'}</span>
        <span className="block truncate font-mono text-[11px] text-muted-foreground">{HAIFY_VERSION}</span>
      </span>
      <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
    </Link>
  );
}

// Jumps to the Resources page filtered by what was typed; ⌘K or Ctrl+K from
// anywhere puts the cursor here.
function SidebarSearch({ onNavigate }: { onNavigate?: () => void }) {
  const navigate = useNavigate();
  const ref = useRef<HTMLInputElement>(null);
  const [q, setQ] = useState('');
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        ref.current?.focus();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);
  return (
    <form
      className="mb-4 px-0.5"
      role="search"
      onSubmit={(e) => {
        e.preventDefault();
        const term = q.trim();
        navigate(term ? `/resources?q=${encodeURIComponent(term)}` : '/resources');
        setQ('');
        ref.current?.blur();
        onNavigate?.();
      }}
    >
      <label className="flex h-9 items-center gap-2 rounded-lg border border-sidebar-border bg-card px-2.5 text-muted-foreground focus-within:border-ring/50">
        <Search className="size-4 shrink-0" />
        <input
          ref={ref}
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="Search resources"
          aria-label="Search resources"
          className="min-w-0 flex-1 bg-transparent text-[13px] text-foreground outline-none placeholder:text-muted-foreground"
        />
        <kbd className="rounded border border-sidebar-border px-1 font-sans text-[10.5px]">⌘K</kbd>
      </label>
    </form>
  );
}

export function MainLayout() {
  const location = useLocation();
  // Live views re-read status every few seconds; announcing each of those
  // would leave the indicator permanently on.
  const isFetching = useIsFetching({ predicate: (q) => !q.meta?.live });
  const [aiOpen, setAiOpen] = useState(false);
  const [navOpen, setNavOpen] = useState(false);
  const [collapsed, setCollapsed] = useState(readCollapsed);

  function toggleCollapsed() {
    setCollapsed((v) => {
      writeCollapsed(!v);
      return !v;
    });
  }

  // Nothing of the Copilot exists until the user asks for it. Once it has been
  // opened it stays mounted even while closed, so its conversation survives
  // toggling the panel shut — the chunk is already downloaded by then, so this
  // costs nothing.
  const aiRequested = useRef(false);
  if (aiOpen) {
    aiRequested.current = true;
  }

  // Close the mobile drawer whenever the route changes (a nav tap navigates).
  useEffect(() => {
    setNavOpen(false);
  }, [location.pathname]);

  // Match the deepest nav item whose path prefixes the current location, so
  // sub-routes (e.g. /ha/create) still show their section title ("HA") and
  // highlight the right nav item instead of falling back to the default.
  const current = navByDepth.find(
    (n) =>
      location.pathname === n.href || location.pathname.startsWith(n.href + '/'),
  );
  useNamePage(current?.name ?? 'Dashboard');

  // ⌘J (Ctrl+J) opens and closes the Copilot from anywhere.
  useEffect(() => {
    if (!AI_ENABLED) return;
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'j') {
        e.preventDefault();
        setAiOpen((v) => !v);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  return (
    <div className="flex h-screen bg-background">
      {/* Desktop sidebar — hidden on phones (< md), collapsible to icons. */}
      <aside
        className={cn(
          'hidden flex-col border-r border-sidebar-border bg-sidebar text-sidebar-foreground transition-[width] duration-200 md:flex',
          collapsed ? 'w-16' : 'w-[244px]',
        )}
      >
        <SidebarContent
          pathname={location.pathname}
          collapsed={collapsed}
          aiOpen={aiOpen}
          onToggleCopilot={() => setAiOpen((v) => !v)}
        />
      </aside>

      {/* Mobile drawer — slides in over the content when the hamburger opens it. */}
      {navOpen && (
        <div className="fixed inset-0 z-50 md:hidden">
          <button
            type="button"
            aria-label="Close menu"
            onClick={() => setNavOpen(false)}
            className="absolute inset-0 bg-black/50"
          />
          <aside className="absolute left-0 top-0 flex h-full w-64 max-w-[80vw] flex-col border-r border-sidebar-border bg-sidebar text-sidebar-foreground shadow-xl">
            <SidebarContent
              pathname={location.pathname}
              onNavigate={() => setNavOpen(false)}
              aiOpen={aiOpen}
              // The Copilot panel would open behind the drawer otherwise.
              onToggleCopilot={() => {
                setNavOpen(false);
                setAiOpen((v) => !v);
              }}
            />
          </aside>
        </div>
      )}

      {/* Main content */}
      <div className="flex flex-1 flex-col overflow-hidden">
        <header className="flex h-14 shrink-0 items-center justify-between px-4 sm:px-6 lg:px-8">
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={() => setNavOpen(true)}
              aria-label="Open menu"
              className="-ml-1 rounded-md p-1.5 text-muted-foreground hover:bg-sidebar-accent hover:text-foreground md:hidden"
            >
              <Menu className="h-5 w-5" />
            </button>
            {/* Desktop counterpart of the hamburger: collapses the sidebar to
                icons rather than opening a drawer. */}
            <button
              type="button"
              onClick={toggleCollapsed}
              aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
              aria-pressed={collapsed}
              title={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
              className="-ml-1 hidden rounded-md p-1.5 text-muted-foreground hover:bg-sidebar-accent hover:text-foreground md:block"
            >
              <PanelLeft className="h-5 w-5" />
            </button>
            {isFetching > 0 && (
              <span
                className="flex items-center gap-1 text-xs text-muted-foreground"
                role="status"
                aria-live="polite"
              >
                <Loader2 className="h-3 w-3 animate-spin" />
                Refreshing
              </span>
            )}
          </div>
          <div className="flex items-center gap-1.5">
            {AI_ENABLED && (
            <button
              type="button"
              onClick={() => setAiOpen((v) => !v)}
              aria-pressed={aiOpen}
              aria-label="Copilot"
              title="Copilot (⌘J)"
              className={cn(
                'flex h-9 items-center gap-2 rounded-[10px] border px-3 text-[13px] font-medium transition-colors',
                aiOpen
                  ? 'border-[color-mix(in_oklch,var(--status-ok)_30%,var(--border))] bg-status-ok-soft text-foreground'
                  : 'border-border bg-card text-foreground/80 hover:text-foreground',
              )}
            >
              <Sparkles className="size-4" />
              Copilot
              <kbd className="hidden rounded border border-border px-1 font-sans text-[10.5px] text-muted-foreground sm:inline">⌘J</kbd>
            </button>
            )}
            <NotificationBell />
            <UserMenu />
          </div>
        </header>
        <main className="app-canvas flex-1 overflow-auto px-4 pt-1 pb-8 sm:px-6 lg:px-8">
          <Suspense fallback={<PageFallback />}>
            <Outlet />
          </Suspense>
        </main>
      </div>

      {AICopilot && aiRequested.current && (
        <Suspense fallback={null}>
          <AICopilot
            open={aiOpen}
            onClose={() => setAiOpen(false)}
            // The desktop sidebar's width (w-16 / w-[244px] above): the panel
            // docks only if the page keeps enough room beside both.
            reservedWidth={collapsed ? 64 : 244}
          />
        </Suspense>
      )}
    </div>
  );
}
