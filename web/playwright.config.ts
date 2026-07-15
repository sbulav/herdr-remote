import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './tests/e2e',
  fullyParallel: true,
  forbidOnly: true,
  retries: 0,
  reporter: 'list',
  use: {
    baseURL: 'http://127.0.0.1:4173',
    trace: 'retain-on-failure',
    connectOptions: process.env.PW_TEST_CONNECT_WS_ENDPOINT
      ? { wsEndpoint: process.env.PW_TEST_CONNECT_WS_ENDPOINT }
      : undefined,
  },
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 4173',
    url: 'http://127.0.0.1:4173/agents',
    reuseExistingServer: true,
  },
  projects: [
    { name: 'phone-chromium', use: { ...devices['Pixel 5'] } },
    { name: 'tablet-chromium', use: { ...devices['Galaxy Tab S4'] } },
    {
      name: 'phone-webkit',
      use: {
        browserName: 'webkit',
        viewport: { width: 390, height: 844 },
        hasTouch: true,
        launchOptions: process.env.PW_WEBKIT_HEADED === '1' ? { headless: false } : undefined,
      },
    },
    {
      name: 'tablet-webkit',
      use: {
        browserName: 'webkit',
        viewport: { width: 834, height: 1194 },
        hasTouch: true,
        launchOptions: process.env.PW_WEBKIT_HEADED === '1' ? { headless: false } : undefined,
      },
    },
  ],
});
