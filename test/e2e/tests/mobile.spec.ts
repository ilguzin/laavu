import { expect, test } from '@playwright/test'
import { readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { agentHome, expectTerm, folderAction, newFolder, termText } from './helpers'
import type { Page } from '@playwright/test'

// Finger swipe down by dy pixels: touchstart, a series of touchmove, touchend.
async function swipe(page: Page, selector: string, dy: number) {
  await page.locator(selector).evaluate(async (el, dy) => {
    const r = el.getBoundingClientRect()
    const x = r.left + r.width / 2
    const y0 = r.top + 40
    const touch = (y: number) => new Touch({ identifier: 1, target: el, clientX: x, clientY: y })
    const fire = (type: string, y: number) =>
      el.dispatchEvent(new TouchEvent(type, { touches: type === 'touchend' ? [] : [touch(y)], changedTouches: [touch(y)], bubbles: true, cancelable: true }))
    fire('touchstart', y0)
    for (let i = 1; i <= 20; i++) {
      fire('touchmove', y0 + (dy * i) / 20)
      await new Promise((r) => setTimeout(r, 16))
    }
    fire('touchend', y0 + dy)
  }, dy)
}

test('phone: screens, terminal input, key strip', async ({ page }) => {
  await page.goto('http://localhost:7700/')
  await expect(page.locator('.mobile-nav')).toBeVisible()
  await expect(page.locator('.rail')).toBeHidden()
  const name = 'Mob' + (Date.now() % 100000)
  await newFolder(page, name)
  await folderAction(page, name, '+ terminal')
  // After a new session: the tab screen with the terminal and key strip.
  await expect(page.locator('.work')).toBeVisible()
  await expect(page.locator('.mob-head')).toContainText(`${name}-1`)
  await expect(page.locator('.keybar')).toBeVisible()
  await expect(page.locator('textarea:not(.xterm-helper-textarea)')).toHaveCount(0)
  await expect(page.locator('.side')).toBeHidden()
  // Input to a freshly started shell is lost until bash is ready, so wait for the prompt.
  await expectTerm(page, `dev/${name}-1`, /\$ *$/m)
  await page.locator('.pane.shown .term').click()
  await page.keyboard.type('echo mob-$((2+3))')
  await page.keyboard.press('Enter')
  await expectTerm(page, `dev/${name}-1`, 'mob-5')
  await page.screenshot({ path: 'test-results/mobile.png' })
  await page.locator('.keys .key', { hasText: '^C' }).click()

  // A swipe on the terminal scrolls tmux history; input afterwards goes to the shell, not to history mode.
  await page.locator('.pane.shown .term').click()
  await page.keyboard.type('seq 1 300')
  await page.keyboard.press('Enter')
  await expectTerm(page, `dev/${name}-1`, /^300$/m)
  await swipe(page, '.pane.shown .term', 260)
  await expectTerm(page, `dev/${name}-1`, /\[\d+\/\d+\]/)
  await page.screenshot({ path: 'test-results/mobile-scroll.png' })
  await page.keyboard.type('echo after-$((40+2))')
  await page.keyboard.press('Enter')
  await expectTerm(page, `dev/${name}-1`, /^after-42$/m)
  // PgUp and PgDn on the key strip: page up enters history mode, page down leaves it.
  await page.locator('.keys .key', { hasText: 'PgUp' }).click()
  await expectTerm(page, `dev/${name}-1`, /\[\d+\/\d+\]/)
  await page.locator('.keys .key', { hasText: 'PgDn' }).click()
  await expect.poll(() => termText(page, `dev/${name}-1`)).not.toMatch(/\[\d+\/\d+\]/)

  // Back to the list: the session is there.
  await page.getByRole('button', { name: 'Back to sessions' }).click()
  await expect(page.locator(`.sess[data-session="${name}-1"]`)).toBeVisible()
  await page.screenshot({ path: 'test-results/mobile-sessions.png' })
  // Files screen: a file opens full screen, gets edited and saved.
  writeFileSync(join(agentHome(7700), name, 'note.md'), '# Note\n')
  await page.locator('.mobile-nav button', { hasText: 'Files' }).click()
  await expect(page.locator('.nav')).toBeVisible()
  await page.locator('.tree .entry[data-path="note.md"]').click()
  await expect(page.locator('.mob-head')).toContainText('note.md')
  await expect(page.locator('.pane.shown .viewer .md h1')).toHaveText('Note')
  await page.locator('.pane.shown .file-head .seg button', { hasText: 'Edit' }).click()
  await page.locator('.pane.shown .cm-content').click()
  await page.keyboard.press('Control+End')
  await page.keyboard.type('from the phone')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect.poll(() => readFileSync(join(agentHome(7700), name, 'note.md'), 'utf8')).toBe('# Note\nfrom the phone')
  await page.screenshot({ path: 'test-results/mobile-edit.png' })
})

// tmux pane width printed by a shell loop: the last W<cols>W value.
async function paneCols(page: Page, key: string): Promise<number> {
  const m = [...(await termText(page, key)).matchAll(/W(\d+)W/g)]
  return m.length ? Number(m[m.length - 1][1]) : 0
}

test('one session on a phone and in a browser: banner and taking the window back', async ({ page, browser }) => {
  const desk = await (await browser.newContext({ viewport: { width: 1500, height: 900 } })).newPage()
  await desk.goto('http://localhost:7700/')
  const name = 'Two' + (Date.now() % 100000)
  await newFolder(desk, name)
  await folderAction(desk, name, '+ terminal')
  const key = `dev/${name}-1`
  await expectTerm(desk, key, /\$ *$/m)
  await desk.locator('.pane.shown .term').click()
  await desk.keyboard.type("while :; do echo W$(tput cols)W; sleep 0.3; done")
  await desk.keyboard.press('Enter')
  const deskCols = await desk.evaluate((k) => (window as any).__laavuTerms[k].cols, key)
  await expect.poll(() => paneCols(desk, key)).toBe(deskCols)

  // The phone opened the same session, so the tmux window shrank to fit it.
  await page.goto(`http://localhost:7700/#/dev/${name}-1`)
  await expect(page.locator('.mob-head')).toContainText(`${name}-1`)
  // Read the phone width after the browser banner: before fitting, xterm sits at 80 columns.
  await expect(desk.locator('.pane.shown .term-elsewhere')).toBeVisible()
  const phoneCols = await page.evaluate((k) => (window as any).__laavuTerms[k].cols, key)
  expect(phoneCols).toBeLessThan(deskCols)
  await expect.poll(() => paneCols(desk, key)).toBe(phoneCols)

  // The browser shows a banner instead of a squeezed screen; "Bring it here" takes the window back,
  // and the banner moves to the phone.
  await page.screenshot({ path: 'test-results/mobile-two-devices.png' })
  await desk.screenshot({ path: 'test-results/desktop-elsewhere.png' })
  await desk.getByRole('button', { name: 'Bring it here' }).click()
  await expect.poll(() => paneCols(desk, key)).toBe(deskCols)
  await expect(desk.locator('.pane.shown .term-elsewhere')).toBeHidden()
  await expect(page.locator('.pane.shown .term-elsewhere')).toBeVisible()
  // Tapping the terminal on the phone without typing takes the window back.
  await page.locator('.pane.shown .term').dispatchEvent('pointerdown')
  await expect.poll(() => paneCols(desk, key)).toBe(phoneCols)
  await expect(page.locator('.pane.shown .term-elsewhere')).toBeHidden()
  await desk.keyboard.press('Control+C')
  await desk.context().close()
})
