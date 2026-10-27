import { expect, type Page } from '@playwright/test'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

export const here = dirname(fileURLToPath(import.meta.url))
export const agentHome = (port: number) => join(here, '..', `.state-${port}`, 'home')

// A tab's terminal text from xterm.js.
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
  try {
    await expect.poll(() => termText(page, key), { timeout: 15_000 }).toMatch(text)
  } catch (e: any) {
    // Say which terminals the page has: an empty text may mean there is no terminal for the key at all.
    const seen = await page.evaluate(() => Object.keys((window as any).__laavuTerms || {}))
    e.message += `\nterminals on the page: ${JSON.stringify(seen)}`
    throw e
  }
}

export async function newFolder(page: Page, name: string) {
  await page.getByRole('button', { name: 'New workspace' }).click()
  await page.fill('.new-folder input', name)
  await page.click('.new-folder button')
  await expect(page.locator(`.folder[data-folder="${name}"]`)).toBeVisible()
}

// A folder button in the session list (visible on hover).
export async function folderAction(page: Page, folder: string, name: string) {
  const f = page.locator(`.folder[data-folder="${folder}"]`)
  await f.locator('.folder-head').hover()
  await f.getByRole('button', { name, exact: true }).click()
}
