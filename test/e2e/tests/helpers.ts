import { expect, type Page } from '@playwright/test'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

export const here = dirname(fileURLToPath(import.meta.url))
export const agentHome = (port: number) => join(here, '..', `.state-${port}`, 'home')

// Текст терминала вкладки из xterm.js.
export async function termText(page: Page, key: string): Promise<string> {
  return page.evaluate((k) => {
    const t = (window as any).__laavuTerms?.[k]
    if (!t) return ''
    const b = t.buffer.active
    const out: string[] = []
    for (let i = 0; i < b.length; i++) out.push(b.getLine(i)?.translateToString(true) ?? '')
    return out.join('\n')
  }, key)
}

export async function expectTerm(page: Page, key: string, text: string | RegExp) {
  await expect.poll(() => termText(page, key), { timeout: 15_000 }).toMatch(text)
}

export async function newFolder(page: Page, name: string) {
  await page.getByRole('button', { name: 'Новая папка' }).click()
  await page.fill('.new-folder input', name)
  await page.click('.new-folder button')
  await expect(page.locator(`.folder[data-folder="${name}"]`)).toBeVisible()
}

// Кнопка папки в списке сессий (видна при наведении).
export async function folderAction(page: Page, folder: string, name: string) {
  const f = page.locator(`.folder[data-folder="${folder}"]`)
  await f.locator('.folder-head').hover()
  await f.getByRole('button', { name, exact: true }).click()
}
