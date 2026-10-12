import { defineConfig, devices } from '@playwright/test';

/** `KIWI_LIVE_E2E=1` runs only the tests tagged `@live` (the real model); otherwise they are left out. */
export const live = process.env['KIWI_LIVE_E2E'] === '1';

export default defineConfig({
  testDir: './tests',
  outputDir: './test-results',
  // One database, one story: the flows build on each other and run in order, in one worker, with no retry.
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: true,
  timeout: 90_000,
  expect: { timeout: 15_000 },
  reporter: [['list']],
  ...(live ? { grep: /@live/ } : { grepInvert: /@live/ }),
  use: {
    baseURL: process.env['KIWI_BASE_URL'] ?? 'http://localhost:8080',
    // A service worker would serve a cached shell and hide a new build; the offline test of flow 1 does not need one.
    serviceWorkers: 'block',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    locale: 'en-US',
    timezoneId: 'Asia/Bangkok',
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } } },
    {
      name: 'phone',
      dependencies: ['desktop'],
      grep: /@phone/,
      use: { ...devices['Desktop Chrome'], viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true },
    },
  ],
});
