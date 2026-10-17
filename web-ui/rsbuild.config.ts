import { execSync } from 'node:child_process';
import { defineConfig } from '@rsbuild/core';

// HAIFY_VERSION when the build sets it, else the checkout's own tag; a
// tarball without git history says "dev".
function haifyVersion(): string {
  if (process.env.HAIFY_VERSION) return process.env.HAIFY_VERSION;
  try {
    return execSync('git describe --tags --always', { stdio: ['ignore', 'pipe', 'ignore'] }).toString().trim();
  } catch {
    return 'dev';
  }
}
import { pluginReact } from '@rsbuild/plugin-react';
import http from 'node:http';

// Create an HTTP/1-only agent for the proxy
// Where `npm run dev` sends API and Copilot calls: a running controller's REST
// gateway and UI port. Override with HAIFY_DEV_REST / HAIFY_DEV_UI.
const devREST = process.env.HAIFY_DEV_REST ?? 'http://127.0.0.1:3375';
const devUI = process.env.HAIFY_DEV_UI ?? 'http://127.0.0.1:3376';

const httpAgent = new http.Agent({
  keepAlive: false,
});

export default defineConfig({
  plugins: [pluginReact()],
  html: {
    title: 'Haify',
    favicon: './src/assets/favicon.svg',
  },
  resolve: {
    alias: {
      '@': './src',
    },
  },
  source: {
    entry: {
      index: './src/index.tsx',
    },
    // The Copilot is built in only with HAIFY_AI=1 (`make build AI=1`).
    // A literal false lets the bundler drop its code, not just hide it.
    define: {
      __HAIFY_AI__: JSON.stringify(process.env.HAIFY_AI === '1'),
      // The release this UI is part of, as `git describe` names it.
      __HAIFY_VERSION__: JSON.stringify(haifyVersion()),
    },
  },
  output: {
    distPath: {
      root: './dist',
    },
  },
  server: {
    port: 3000,
    // The SPA calls the controller's REST gateway at /v1/* directly and the
    // Copilot at /ai/* (served from the controller's UI port), so a dev
    // session proxies both prefixes to a running controller.
    proxy: {
      '/v1': {
        target: devREST,
        changeOrigin: true,
        agent: httpAgent,
        // Force HTTP/1.1
        protocol: 'http',
      },
      '/ai': {
        target: devUI,
        changeOrigin: true,
        agent: httpAgent,
        protocol: 'http',
      },
    },
  },
});
