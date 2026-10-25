import { expect, test } from '@playwright/test'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { here } from './helpers'

test('регистрация passkey по ссылке и вход', async ({ page, context }) => {
  const cdp = await context.newCDPSession(page)
  await cdp.send('WebAuthn.enable')
  await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
  })
  const log = readFileSync(join(here, '..', '.server-7800.log'), 'utf8')
  const m = log.match(/(http:\/\/localhost:7800\/register#\S+)/)
  expect(m, 'ссылка регистрации в выводе laavud dev --auth').toBeTruthy()

  // Без входа — экран входа.
  await page.goto('http://localhost:7800/')
  await expect(page.getByRole('button', { name: 'Войти с passkey' })).toBeVisible()

  await page.goto(m![1])
  await page.fill('.auth input', 'Денис')
  await page.getByRole('button', { name: 'Создать passkey' }).click()
  await expect(page.locator('.side')).toBeVisible()
  await expect(page).toHaveURL('http://localhost:7800/')

  // Ссылка одноразовая.
  const again = await page.request.post('http://localhost:7800/api/auth/register/check', { data: { token: m![1].split('#')[1] }, headers: { Origin: 'http://localhost:7800' } })
  expect(again.status()).toBe(400)

  // Выход и вход тем же passkey.
  await page.getByRole('button', { name: 'Меню', exact: true }).click()
  await page.getByRole('button', { name: 'Выйти' }).click()
  await expect(page.getByRole('button', { name: 'Войти с passkey' })).toBeVisible()
  await page.getByRole('button', { name: 'Войти с passkey' }).click()
  await expect(page.locator('.side')).toBeVisible()

  // Ссылка для второго устройства.
  await page.getByRole('button', { name: 'Меню', exact: true }).click()
  await page.getByRole('button', { name: 'Добавить устройство' }).click()
  await expect(page.locator('.modal-device input')).toHaveValue(/\/register#/)
})
