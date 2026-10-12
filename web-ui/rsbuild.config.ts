import { defineConfig } from '@rsbuild/core';
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
