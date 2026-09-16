import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Dev server proxies admin API and data plane to the Go backend, so no CORS
// is involved; `make build` embeds dist into the binary.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:8080',
      '/v1': 'http://127.0.0.1:8080',
    },
  },
  build: { outDir: 'dist' },
});
