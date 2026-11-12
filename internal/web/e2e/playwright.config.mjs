import { defineConfig } from '@playwright/test';

/*
  One command runs everything:   npm --prefix internal/web/e2e test

  globalSetup builds cmd/repocortex and starts one portal per fixture repository;
  globalTeardown stops only the processes it started. Nothing here touches a
  port the developer already has open.
*/
export default defineConfig({
  testDir: './specs',
  globalSetup: './support/global-setup.mjs',
  globalTeardown: './support/global-teardown.mjs',
  timeout: 90_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [['list'], ['json', { outputFile: 'results.json' }]],
  use: {
    viewport: { width: 1440, height: 900 },
    actionTimeout: 15_000,
    trace: 'retain-on-failure',
  },
});
