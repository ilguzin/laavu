import { defineConfig, devices } from '@playwright/test'
import { existsSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'

// Сквозная проверка установленного сервера на тестовой ВМ: Caddy, TLS внутреннего
// CA, passkey с RP ID localhost. Запуск — после test/vm/e2e-server.sh:
//   LAAVU_LINK=$(cat ../vm/.last-register-link) npx playwright test -c playwright.vm.config.ts
const libs = join(homedir(), '.local/opt/pw-libs/usr/lib/x86_64-linux-gnu')
const launchOptions = existsSync(libs)
  ? { env: { ...process.env, LD_LIBRARY_PATH: [libs, process.env.LD_LIBRARY_PATH].filter(Boolean).join(':') } as Record<string, string> }
  : {}

export default defineConfig({
  testDir: './vm',
  timeout: 120_000,
  expect: { timeout: 15_000 },
  workers: 1,
  reporter: [['list']],
  use: {
    ...devices['Desktop Chrome'],
    viewport: { width: 1500, height: 900 },
    ignoreHTTPSErrors: true,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    launchOptions,
  },
})
