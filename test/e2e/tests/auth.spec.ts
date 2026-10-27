import { expect, test } from '@playwright/test'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { here } from './helpers'

test('passkey registration via link and sign-in', async ({ page, context }) => {
  const cdp = await context.newCDPSession(page)
  await cdp.send('WebAuthn.enable')
  await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
  })
  const log = readFileSync(join(here, '..', '.server-7800.log'), 'utf8')
  const m = log.match(/(http:\/\/localhost:7800\/register#\S+)/)
  expect(m, 'registration link in laavud dev --auth output').toBeTruthy()

  // Not signed in: the sign-in screen.
  await page.goto('http://localhost:7800/')
  await expect(page.getByRole('button', { name: 'Sign in with passkey' })).toBeVisible()

  await page.goto(m![1])
  await page.fill('.auth input', 'Alice')
  await page.getByRole('button', { name: 'Create passkey' }).click()
  await expect(page.locator('.side')).toBeVisible()
  await expect(page).toHaveURL('http://localhost:7800/')

  // The link is single-use.
  const again = await page.request.post('http://localhost:7800/api/auth/register/check', { data: { token: m![1].split('#')[1] }, headers: { Origin: 'http://localhost:7800' } })
  expect(again.status()).toBe(400)

  // Sign out and sign in with the same passkey.
  await page.getByRole('button', { name: 'Menu', exact: true }).click()
  await page.getByRole('button', { name: 'Sign out' }).click()
  await expect(page.getByRole('button', { name: 'Sign in with passkey' })).toBeVisible()
  await page.getByRole('button', { name: 'Sign in with passkey' }).click()
  await expect(page.locator('.side')).toBeVisible()

  // Link for a second device.
  await page.getByRole('button', { name: 'Menu', exact: true }).click()
  await page.getByRole('button', { name: 'Add device' }).click()
  await expect(page.locator('.modal-device input')).toHaveValue(/\/register#/)
})
