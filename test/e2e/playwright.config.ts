import { defineConfig, devices } from '@playwright/test'
import { existsSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'

// Хост без sudo: недостающие библиотеки Chromium распакованы из .deb в ~/.local/opt/pw-libs
// (см. README e2e). Если каталога нет — браузер берёт системные.
const libs = join(homedir(), '.local/opt/pw-libs/usr/lib/x86_64-linux-gnu')
const launchOptions = existsSync(libs)
  ? { env: { ...process.env, LD_LIBRARY_PATH: [libs, process.env.LD_LIBRARY_PATH].filter(Boolean).join(':') } as Record<string, string> }
  : {}

export default defineConfig({
  testDir: './tests',
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: 1,
  reporter: [['list']],
  globalTeardown: './teardown.ts',
  use: {
    trace: 'retain-on-failure',
    launchOptions,
    screenshot: 'only-on-failure',
  },
  webServer: [
    { command: './start.sh 7700', url: 'http://localhost:7700/api/me', reuseExistingServer: false, timeout: 30_000 },
    { command: './start.sh 7800 --auth', url: 'http://localhost:7800/api/me', reuseExistingServer: false, timeout: 30_000 },
  ],
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'], viewport: { width: 1500, height: 900 } }, testIgnore: /mobile/ },
    { name: 'mobile', use: { ...devices['Pixel 7'] }, testMatch: /mobile/ },
  ],
})
