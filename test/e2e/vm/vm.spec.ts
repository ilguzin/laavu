import { expect, test } from '@playwright/test'
import { expectTerm } from '../tests/helpers'

// Адрес сервера: по умолчанию проброс на localhost, или имя в tailnet (LAAVU_APP).
const APP = process.env.LAAVU_APP || 'https://localhost:8443'
const HOST = new URL(APP).hostname.replace(/\./g, '\\.')

test('сервер на ВМ: passkey, сессии через хуки, просмотр, dev-сервер', async ({ page, context }) => {
  const link = process.env.LAAVU_LINK
  expect(link, 'LAAVU_LINK — ссылка регистрации из laavu install').toBeTruthy()
  const problems: string[] = []
  page.on('pageerror', (e) => problems.push('pageerror: ' + e.message))
  page.on('console', (m) => {
    if (/Content Security Policy/i.test(m.text())) problems.push(m.text())
  })
  const cdp = await context.newCDPSession(page)
  await cdp.send('WebAuthn.enable')
  await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
  })

  // Без входа API закрыт.
  const anon = await page.request.get(APP + '/api/agents')
  expect(anon.status()).toBe(401)

  await page.goto(link!)
  await page.fill('.auth input', 'Денис')
  await page.getByRole('button', { name: 'Создать passkey' }).click()
  await expect(page.locator('.sidebar')).toBeVisible()

  // Время последней проверки изоляции видно администратору.
  await page.locator('.menu-wrap > button').click()
  await expect(page.getByTestId('isolation')).toContainText('всё закрыто')
  await page.locator('.menu-wrap > button').click()

  // Сессия claude (заглушка): статус «ждёт ответа» приходит через хуки агента и сокет демона.
  await page.fill('.new-folder input', 'Site')
  await page.click('.new-folder button')
  const folder = page.locator('.folder[data-folder="Site"]')
  await expect(folder).toBeVisible()
  if (!(await folder.locator('.folder-body').isVisible())) await folder.locator('.folder-row').click()
  await folder.getByRole('button', { name: '+ сессия' }).click()
  await expect(page.locator('.sess[data-session="Site-1"] .dot')).toHaveClass(/st-waiting/, { timeout: 20_000 })
  await page.fill('.input textarea', 'привет с ВМ')
  await page.getByRole('button', { name: 'Отправить' }).click()
  await expectTerm(page, 'demo/Site-1', 'echo: привет с ВМ')

  // Терминал через WSS и Caddy; агент пишет html и поднимает dev-сервер на своём порту.
  await folder.getByRole('button', { name: '+ терминал' }).click()
  await expect(page.locator('.tab[data-tab="demo/Site-2"]')).toBeVisible()
  await page.locator('.pane.shown .term').click()
  await page.keyboard.type(`echo '<p id=r>vm-ok</p>' > page.html && python3 -m http.server 20001 --bind 127.0.0.1`)
  await page.keyboard.press('Enter')
  await expectTerm(page, 'demo/Site-2', 'Serving HTTP')

  await page.locator('.tree .entry[data-path="page.html"]').click()
  await expect(page.locator('iframe.html-view')).toHaveAttribute('src', new RegExp(`^https://${HOST}:8444/t/`))
  await expect(page.frameLocator('iframe.html-view').locator('#r')).toHaveText('vm-ok')

  page.once('dialog', (d) => d.accept('20001'))
  const popupP = context.waitForEvent('page')
  await page.getByTitle('Открыть dev-сервер агента').click()
  const popup = await popupP
  await popup.waitForLoadState()
  expect(popup.url()).toMatch(new RegExp(`^https://${HOST}:845\\d/$`))
  await expect(popup.locator('body')).toContainText('Directory listing')
  await expect(popup.locator('body')).toContainText('page.html')

  // Порт вне диапазона агента не открывается.
  const bad = await page.request.post(APP + '/api/agents/demo/ports/30000/open', { headers: { Origin: APP } })
  expect(bad.status()).toBe(400)
  // Чужой Origin не проходит даже с cookie.
  const evil = await page.request.post(APP + '/api/agents/demo/workdirs', { headers: { Origin: 'https://evil.example' }, data: { name: 'x' } })
  expect(evil.status()).toBe(403)
  expect(problems).toEqual([])
})
