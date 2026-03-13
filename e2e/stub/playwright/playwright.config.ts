import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: 1,
  reporter: [
    ['html', { open: 'never' }],
    ['list']
  ],
  use: {
    baseURL: process.env.API_BASE_URL || 'http://localhost:9000',
    trace: 'on-first-retry',
  },
  projects: [
    {
      name: 'api-tests',
      testMatch: /api\.spec\.ts/,
      use: {},
    },
    {
      name: 'ui-tests',
      testMatch: /ui\.spec\.ts/,
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  webServer: process.env.START_SERVER === 'true' ? {
    command: 'node server.js',
    url: 'http://localhost:9000/health',
    reuseExistingServer: !process.env.CI,
    timeout: 30000,
  } : undefined,
});
