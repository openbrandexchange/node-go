import { defineConfig, devices } from '@playwright/test'

// Walks the real brand admin and retailer portal against a running node-go.
// Start a fresh node first (see README.md in this folder) and point OBX_E2E_URL at it.
// "flows" sets the node up and walks every task; "accessibility" then checks
// the same screens with axe and the keyboard.
export default defineConfig({
  timeout: 180_000,
  expect: { timeout: 20_000 },
  workers: 1,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    baseURL: process.env.OBX_E2E_URL ?? 'http://localhost:18080',
    locale: 'en-GB',
    trace: 'retain-on-failure',
    actionTimeout: 20_000,
    ...devices['Desktop Chrome'],
  },
  projects: [
    { name: 'flows', testDir: '.', testMatch: 'flows.spec.ts' },
    { name: 'accessibility', testDir: '.', testMatch: 'accessibility.spec.ts', dependencies: ['flows'] },
  ],
})
