import { defineConfig } from 'vite';

// Gate (b) page. Run from scripts/gates: `npm run gate:b` (vite buyin --config buyin/vite.config.ts).
// Bound to loopback only: the page holds a throwaway test key in memory.
export default defineConfig({
  server: { host: '127.0.0.1', port: 5173, strictPort: true, open: false },
  preview: { host: '127.0.0.1', port: 5173, strictPort: true },
});
