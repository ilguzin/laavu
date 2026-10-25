import { expect, test } from '@playwright/test'
import { readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { agentHome, expectTerm, folderAction, newFolder, termText } from './helpers'
import type { Page } from '@playwright/test'

// Свайп пальцем вниз на dy пикселей: touchstart, серия touchmove, touchend.
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

test('телефон: экраны, ввод в терминал, полоса клавиш', async ({ page }) => {
  await page.goto('http://localhost:7700/')
  await expect(page.locator('.mobile-nav')).toBeVisible()
  await expect(page.locator('.rail')).toBeHidden()
  const name = 'Mob' + (Date.now() % 100000)
  await newFolder(page, name)
  await folderAction(page, name, '+ терминал')
  // После новой сессии — экран вкладки с терминалом и полосой клавиш.
  await expect(page.locator('.work')).toBeVisible()
  await expect(page.locator('.mob-head')).toContainText(`${name}-1`)
  await expect(page.locator('.keybar')).toBeVisible()
  await expect(page.locator('textarea:not(.xterm-helper-textarea)')).toHaveCount(0)
  await expect(page.locator('.side')).toBeHidden()
  // Ввод в только что запущенный shell теряется, пока bash не готов, — ждём приглашение.
  await expectTerm(page, `dev/${name}-1`, /\$ *$/m)
  await page.locator('.pane.shown .term').click()
  await page.keyboard.type('echo mob-$((2+3))')
  await page.keyboard.press('Enter')
  await expectTerm(page, `dev/${name}-1`, 'mob-5')
  await page.screenshot({ path: 'test-results/mobile.png' })
  await page.locator('.keys .key', { hasText: '^C' }).click()

  // Свайп по терминалу прокручивает историю tmux; ввод после этого идёт в shell, а не в просмотр истории.
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
  // PgUp и PgDn на полосе клавиш: страница вверх — просмотр истории, страница вниз — обратно.
  await page.locator('.keys .key', { hasText: 'PgUp' }).click()
  await expectTerm(page, `dev/${name}-1`, /\[\d+\/\d+\]/)
  await page.locator('.keys .key', { hasText: 'PgDn' }).click()
  await expect.poll(() => termText(page, `dev/${name}-1`)).not.toMatch(/\[\d+\/\d+\]/)

  // Назад к списку: сессия там.
  await page.getByRole('button', { name: 'К сессиям' }).click()
  await expect(page.locator(`.sess[data-session="${name}-1"]`)).toBeVisible()
  await page.screenshot({ path: 'test-results/mobile-sessions.png' })
  // Экран файлов: файл открывается на весь экран, правится и сохраняется.
  writeFileSync(join(agentHome(7700), name, 'note.md'), '# Заметка\n')
  await page.locator('.mobile-nav button', { hasText: 'Файлы' }).click()
  await expect(page.locator('.nav')).toBeVisible()
  await page.locator('.tree .entry[data-path="note.md"]').click()
  await expect(page.locator('.mob-head')).toContainText('note.md')
  await expect(page.locator('.pane.shown .viewer .md h1')).toHaveText('Заметка')
  await page.locator('.pane.shown .file-head .seg button', { hasText: 'Правка' }).click()
  await page.locator('.pane.shown .cm-content').click()
  await page.keyboard.press('Control+End')
  await page.keyboard.type('с телефона')
  await page.getByRole('button', { name: 'Сохранить' }).click()
  await expect.poll(() => readFileSync(join(agentHome(7700), name, 'note.md'), 'utf8')).toBe('# Заметка\nс телефона')
  await page.screenshot({ path: 'test-results/mobile-edit.png' })
})

// Ширина панели tmux, которую печатает цикл в shell: последнее значение W<cols>W.
async function paneCols(page: Page, key: string): Promise<number> {
  const m = [...(await termText(page, key)).matchAll(/W(\d+)W/g)]
  return m.length ? Number(m[m.length - 1][1]) : 0
}

test('одна сессия на телефоне и в браузере: плашка и возврат окна', async ({ page, browser }) => {
  const desk = await (await browser.newContext({ viewport: { width: 1500, height: 900 } })).newPage()
  await desk.goto('http://localhost:7700/')
  const name = 'Two' + (Date.now() % 100000)
  await newFolder(desk, name)
  await folderAction(desk, name, '+ терминал')
  const key = `dev/${name}-1`
  await expectTerm(desk, key, /\$ *$/m)
  await desk.locator('.pane.shown .term').click()
  await desk.keyboard.type("while :; do echo W$(tput cols)W; sleep 0.3; done")
  await desk.keyboard.press('Enter')
  const deskCols = await desk.evaluate((k) => (window as any).__laavuTerms[k].cols, key)
  await expect.poll(() => paneCols(desk, key)).toBe(deskCols)

  // Телефон открыл ту же сессию — окно tmux сжалось под него.
  await page.goto(`http://localhost:7700/#/dev/${name}-1`)
  await expect(page.locator('.mob-head')).toContainText(`${name}-1`)
  // Ширину телефона читаем после плашки в браузере: до подгонки xterm стоит 80 колонок.
  await expect(desk.locator('.pane.shown .term-elsewhere')).toBeVisible()
  const phoneCols = await page.evaluate((k) => (window as any).__laavuTerms[k].cols, key)
  expect(phoneCols).toBeLessThan(deskCols)
  await expect.poll(() => paneCols(desk, key)).toBe(phoneCols)

  // В браузере вместо сжатого экрана — плашка; «Вернуть сюда» возвращает окно,
  // и плашка переходит на телефон.
  await page.screenshot({ path: 'test-results/mobile-two-devices.png' })
  await desk.screenshot({ path: 'test-results/desktop-elsewhere.png' })
  await desk.getByRole('button', { name: 'Вернуть сюда' }).click()
  await expect.poll(() => paneCols(desk, key)).toBe(deskCols)
  await expect(desk.locator('.pane.shown .term-elsewhere')).toBeHidden()
  await expect(page.locator('.pane.shown .term-elsewhere')).toBeVisible()
  // Касание терминала на телефоне без ввода забирает окно обратно.
  await page.locator('.pane.shown .term').dispatchEvent('pointerdown')
  await expect.poll(() => paneCols(desk, key)).toBe(phoneCols)
  await expect(page.locator('.pane.shown .term-elsewhere')).toBeHidden()
  await desk.keyboard.press('Control+C')
  await desk.context().close()
})
