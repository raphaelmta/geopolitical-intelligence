import { configDefaults, defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// The built index.html must stay compatible with the CSP served by nginx
// (script-src 'self', style-src 'self'): no inline scripts or styles, no
// data: URLs for assets.
export default defineConfig({
  plugins: [react()],
  build: {
    modulePreload: { polyfill: false },
    assetsInlineLimit: 0,
    sourcemap: false,
  },
  test: {
    environment: 'jsdom',
    // e2e/ is the Playwright suite (make e2e), not unit tests; e2e-results/ is its scratch output.
    exclude: [...configDefaults.exclude, 'e2e/**', 'e2e-results/**'],
    setupFiles: ['./src/test/setup.ts'],
    restoreMocks: true,
    unstubGlobals: true,
    unstubEnvs: true,
  },
});
