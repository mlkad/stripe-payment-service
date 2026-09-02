import { fileURLToPath, URL } from 'node:url';
import { defineConfig, loadEnv } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '');
  const target = env.VITE_DEV_PROXY_TARGET || 'http://localhost:8080';

  return {
    plugins: [react(), tailwindcss()],
    resolve: {
      // Declared here as well as in tsconfig: tsconfig paths drive the type
      // checker, this drives the bundler. Relying on one to cover the other
      // works until it doesn't - a CSS import through the alias resolves at
      // build time but not in dev.
      alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
    },
    server: {
      port: 5173,
      // Proxying /api in development keeps the browser on one origin, so CORS
      // is exercised only by deployments that genuinely serve the UI elsewhere.
      proxy: { '/api': { target, changeOrigin: true } },
    },
  };
});
