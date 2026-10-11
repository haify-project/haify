import { lazy } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { BrowserRouter, Routes, Route, Navigate } from 'react-router';
import { TooltipProvider } from '@/components/ui/tooltip';
import { Toaster } from '@/components/ui/sonner';
import { AuthTokenDialog } from '@/components/AuthTokenDialog';
import { MainLayout } from './layouts/MainLayout';

// Pages are code-split so the first paint only downloads the shell and the one
// route being visited. Loading them eagerly meant every visitor paid for every
// page — including CreateHAPage's drag-and-drop stack — before seeing anything,
// which is a second on the LAN and far worse over a long-haul link.
const DashboardPage = lazy(() =>
  import('./pages/DashboardPage').then((m) => ({ default: m.DashboardPage })),
);
const NodesPage = lazy(() =>
  import('./pages/NodesPage').then((m) => ({ default: m.NodesPage })),
);
const PoolsPage = lazy(() =>
  import('./pages/PoolsPage').then((m) => ({ default: m.PoolsPage })),
);
const ResourcesPage = lazy(() =>
  import('./pages/ResourcesPage').then((m) => ({ default: m.ResourcesPage })),
);
const GatewaysPage = lazy(() =>
  import('./pages/GatewaysPage').then((m) => ({ default: m.GatewaysPage })),
);
const HAPage = lazy(() =>
  import('./pages/HAPage').then((m) => ({ default: m.HAPage })),
);
const CreateHAPage = lazy(() =>
  import('./pages/CreateHAPage').then((m) => ({ default: m.CreateHAPage })),
);
const AccessPage = lazy(() =>
  import('./pages/AccessPage').then((m) => ({ default: m.AccessPage })),
);
const NotificationsPage = lazy(() =>
  import('./pages/NotificationsPage').then((m) => ({
    default: m.NotificationsPage,
  })),
);
const LogsPage = lazy(() =>
  import('./pages/LogsPage').then((m) => ({ default: m.LogsPage })),
);
const LiveMapPage = lazy(() =>
  import('./pages/live/LiveMapPage').then((m) => ({ default: m.LiveMapPage })),
);
const ProxmoxPage = lazy(() =>
  import('./pages/ProxmoxPage').then((m) => ({ default: m.ProxmoxPage })),
);
const KvmPage = lazy(() => import('./pages/KvmPage').then((m) => ({ default: m.KvmPage })));
const SettingsPage = lazy(() =>
  import('./pages/SettingsPage').then((m) => ({ default: m.SettingsPage })),
);
// The Suspense boundary lives in MainLayout, around the Outlet, so the sidebar
// and header stay put while a route's chunk arrives.

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // This is a console for infrastructure that moves on its own: nodes fail
      // over, resources change primary, the controller relocates. Returning to
      // a backgrounded tab must not show the state from before you left —
      // acting on it is how you evict a node that is no longer the active one.
      // staleTime keeps this from becoming a refetch storm: data younger than
      // five seconds is left alone.
      refetchOnWindowFocus: true,
      retry: 1,
      staleTime: 5000,
    },
  },
});

function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <BrowserRouter>
          <Routes>
            <Route path="/" element={<MainLayout />}>
              <Route index element={<Navigate to="/dashboard" replace />} />
              <Route path="dashboard" element={<DashboardPage />} />
              <Route path="live" element={<LiveMapPage />} />
              <Route path="3d" element={<Navigate to="/live" replace />} />
              <Route path="nodes" element={<NodesPage />} />
              <Route path="pools" element={<PoolsPage />} />
              <Route path="resources" element={<ResourcesPage />} />
              <Route path="proxmox" element={<ProxmoxPage />} />
              <Route path="kvm" element={<KvmPage />} />
              {/* Profiles moved under Resources; keep the old path working
                  rather than 404ing links and bookmarks that already exist. */}
              <Route
                path="resource-profiles"
                element={<Navigate to="/resources?tab=profiles" replace />}
              />
              <Route path="gateways" element={<GatewaysPage />} />
              <Route path="ha" element={<HAPage />} />
              <Route path="ha/create" element={<CreateHAPage />} />
              <Route
                path="notifications"
                element={<NotificationsPage />}
              />
              <Route path="access" element={<AccessPage />} />
              <Route path="logs" element={<LogsPage />} />
              <Route path="settings" element={<SettingsPage />} />
            </Route>
          </Routes>
        </BrowserRouter>
        <AuthTokenDialog />
        {/* Bottom-right, not top-right: the header's right edge holds the
            notification bell, the Copilot toggle and the account menu, and a
            toast stack parked over them covers the controls it is telling you
            to go and use. */}
        <Toaster richColors position="bottom-right" />
      </TooltipProvider>
    </QueryClientProvider>
  );
}

export default App;
