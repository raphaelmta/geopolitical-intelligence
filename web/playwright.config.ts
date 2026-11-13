// End-to-end test of the whole stack (`make e2e`): runs in the official
// Playwright container on the network of the throwaway compose project
// geoint-e2e (deploy/compose.e2e.yml), never against a dev stack.
import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './e2e',
  // Git-ignored; traces and screenshots are kept only for failed tests.
  outputDir: './e2e-results/artifacts',
  // One browser, one test at a time: the tests share the seeded database.
  workers: 1,
  fullyParallel: false,
  // A flaky end-to-end test is a defect to fix, not to retry.
  retries: 0,
  forbidOnly: true,
  timeout: 120_000,
  globalTimeout: 10 * 60_000,
  expect: { timeout: 15_000 },
  reporter: [['list'], ['html', { outputFolder: './e2e-results/report', open: 'never' }]],
  use: {
    baseURL: 'http://localhost:5173',
    locale: 'pt-BR',
    timezoneId: 'America/Sao_Paulo',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'off',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    // Loopback forwarders to web, api and keycloak (see e2e/forward.mjs).
    command: 'node e2e/forward.mjs',
    url: 'http://localhost:5173/config.json',
    reuseExistingServer: false,
    timeout: 60_000,
    stdout: 'pipe',
  },
});
