import { expect, test } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { agentHome, expectTerm, folderAction, newFolder } from './helpers'

const URL = 'http://localhost:7700/'

// Минимальный PDF на одну страницу с текстом.
function tinyPdf(text: string): string {
  const objs = [
    '<< /Type /Catalog /Pages 2 0 R >>',
    '<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 200] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>',
    '',
    '<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>',
  ]
  const stream = `BT /F1 24 Tf 40 100 Td (${text}) Tj ET`
  objs[3] = `<< /Length ${stream.length} >>\nstream\n${stream}\nendstream`
  let out = '%PDF-1.4\n'
  const offs: number[] = []
  objs.forEach((o, i) => {
    offs.push(out.length)
    out += `${i + 1} 0 obj\n${o}\nendobj\n`
  })
  const xref = out.length
  out += `xref\n0 ${objs.length + 1}\n0000000000 65535 f \n` + offs.map((o) => `${String(o).padStart(10, '0')} 00000 n \n`).join('')
  out += `trailer\n<< /Size ${objs.length + 1} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`
  return out
}

const shown = '.pane.shown'

test('сессия агента: статус, ввод в терминал, файлы во вкладках, просмотр, загрузка, изменения', async ({ page }) => {
  const problems: string[] = []
  page.on('pageerror', (e) => problems.push('pageerror: ' + e.message))
  page.on('console', (m) => {
    if (/Content Security Policy/i.test(m.text()) || (m.type() === 'error' && !/Failed to load resource: .* 422/.test(m.text()))) problems.push(m.text())
  })
  await page.goto(URL)
  await newFolder(page, 'Proj')
  await folderAction(page, 'Proj', '+ сессия')
  await expect(page.locator('.tab[data-tab="dev/Proj-1"]')).toBeVisible()
  // SessionStart заглушки → «ждёт ответа», сессия в блоке «Ждут тебя».
  await expect(page.locator('.sess[data-session="Proj-1"] .dot')).toHaveClass(/st-waiting/, { timeout: 15_000 })
  await expect(page.locator('.wait-card[data-waiting="Proj-1"]')).toBeVisible()
  await expectTerm(page, 'dev/Proj-1', /fakeclaude .* \(startup\)/)

  // Поля ввода нет — пишем прямо в терминал.
  await expect(page.locator('textarea:not(.xterm-helper-textarea)')).toHaveCount(0)
  await page.locator(`${shown} .term`).click()
  await page.keyboard.type('привет')
  await page.keyboard.press('Enter')
  await expectTerm(page, 'dev/Proj-1', 'echo: привет')
  // Индикатор контекста после хода.
  await expect(page.locator('.sess-head .ctx')).toContainText('контекст')

  // Файл, написанный агентом, появляется в дереве по inotify и открывается во вкладке.
  // Папка с git, заведённая мимо клиента: после хода список папок перечитывается.
  const gitDir = join(agentHome(7700), 'Gitty')
  mkdirSync(gitDir, { recursive: true })
  execFileSync('git', ['init', '-q', '-b', 'trunk', gitDir])
  writeFileSync(join(gitDir, 'a.txt'), 'a')
  await page.keyboard.type('write:x.md')
  await page.keyboard.press('Enter')
  await expect(page.locator('.tree .entry[data-path="x.md"]')).toBeVisible()
  // Последнее действие агента — в списке сессий; ветка и изменения — у папки.
  await expect(page.locator('.sess[data-session="Proj-1"] .act')).toHaveText(' · Write x.md')
  await expect(page.locator('.folder[data-folder="Gitty"] .branch')).toHaveText('trunk')
  await expect(page.locator('.folder[data-folder="Gitty"] .chg-count')).toHaveText('±1')
  await page.locator('.tree .entry[data-path="x.md"]').click()
  await expect(page.locator(`${shown} .viewer .md`)).toContainText('fake')
  await expect(page.locator('.tab.file.preview[data-file="x.md"]')).toBeVisible()

  // Markdown с формулой, mermaid и html на origin просмотра.
  const home = agentHome(7700)
  mkdirSync(join(home, 'Proj', 'docs'), { recursive: true })
  writeFileSync(join(home, 'Proj', 'docs', 'note.md'), '# Заголовок\n\nФормула $x^2$.\n\n![pic](pic.svg)\n\n```mermaid\ngraph TD; A-->B\n```\n')
  writeFileSync(join(home, 'Proj', 'docs', 'pic.svg'), '<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10"/></svg>')
  writeFileSync(join(home, 'Proj', 'page.html'), '<!doctype html><body><div id="r"></div><script>document.getElementById("r").textContent="html-" + (6*7)</script></body>')
  await page.locator('.tree .entry[data-path="docs"]').click()
  await page.locator('.tree .entry[data-path="docs/note.md"]').click()
  // Временная вкладка заменилась, а не добавилась.
  await expect(page.locator('.tab.file')).toHaveCount(1)
  await expect(page.locator(`${shown} .viewer .md h1`)).toHaveText('Заголовок')
  await expect(page.locator(`${shown} .viewer .md .katex`).first()).toBeVisible()
  await expect(page.locator(`${shown} .viewer .md img`)).toHaveAttribute('src', /^http:\/\/localhost:7701\/t\/[^/]+\/docs\/pic\.svg$/)
  await expect(page.locator(`${shown} .viewer .md .mermaid svg`)).toBeVisible({ timeout: 20_000 })
  // Двойной щелчок закрепляет вкладку.
  await page.locator('.tab.file[data-file="docs/note.md"]').dblclick()
  await expect(page.locator('.tab.file.preview')).toHaveCount(0)
  // Режим «Исходник» с номерами строк.
  await page.locator(`${shown} .file-head .seg button`, { hasText: 'Исходник' }).click()
  await expect(page.locator(`${shown} .viewer .ln[data-line="1"]`)).toContainText('# Заголовок')
  // Выделение строки → «в сессию»: цитата вставляется в терминал.
  await page.locator(`${shown} .viewer .ln[data-line="3"] .src`).selectText()
  await page.locator(`${shown} .viewer-body`).dispatchEvent('mouseup')
  await page.getByRole('button', { name: 'в сессию' }).click()
  // Сессия не была на экране — её вкладка стала активной.
  await expect(page.locator('.tab.on[data-tab="dev/Proj-1"]')).toBeVisible()
  await expectTerm(page, 'dev/Proj-1', /Proj\/docs\/note\.md:3/)
  await page.locator('.tab.file[data-file="docs/note.md"]').click()
  await page.locator(`${shown} .file-head .seg button`, { hasText: 'Просмотр' }).click()

  // Новый файл открывается рядом с закреплённым.
  await page.locator('.tree .entry[data-path="page.html"]').click()
  await expect(page.locator('.tab.file')).toHaveCount(2)
  const frame = page.frameLocator(`${shown} iframe.html-view`)
  await expect(frame.locator('#r')).toHaveText('html-42')
  await expect(page.locator(`${shown} iframe.html-view`)).toHaveAttribute('src', /^http:\/\/localhost:7701\//)

  // PDF через pdf.js по URL просмотра.
  writeFileSync(join(home, 'Proj', 'doc.pdf'), tinyPdf('Laavu PDF'))
  await page.locator('.tree .entry[data-path="doc.pdf"]').click()
  await expect(page.locator(`${shown} .viewer canvas.pdf-page`)).toBeVisible({ timeout: 20_000 })
  await page.screenshot({ path: 'test-results/desktop.png' })

  // Загрузка кнопкой в шапке сессии: путь — в терминал.
  await page.locator('.tab[data-tab="dev/Proj-1"]').click()
  await page.locator(`${shown} .sess-head input[type=file]`).setInputFiles({ name: 'photo.jpg', mimeType: 'image/jpeg', buffer: Buffer.from('JPEG') })
  await expectTerm(page, 'dev/Proj-1', 'photo.jpg')
  await expect(page.locator('.tree .entry[data-path="photo.jpg"]')).toBeVisible()

  // Что изменилось (без git — по времени) — вкладка навигатора.
  await folderAction(page, 'Proj', 'что изменилось')
  await expect(page.getByTestId('changes')).toContainText('x.md')
  await expect(page.getByTestId('changes')).toContainText('photo.jpg')
  await page.getByRole('button', { name: 'Отметить как просмотренное' }).click()
  await expect(page.getByTestId('changes')).toContainText('Ничего не изменилось')

  // История панели.
  await page.getByRole('button', { name: 'История', exact: true }).click()
  await expect(page.locator('.modal-history .xterm')).toBeVisible()
  await page.locator('.modal-close').click()
  await page.screenshot({ path: 'test-results/desktop-session.png' })
  expect(problems, 'ошибки в консоли и нарушения CSP').toEqual([])
})

test('терминал: ввод прямо в xterm, вкладки рядом, вставка без Enter', async ({ page }) => {
  await page.goto(URL)
  await newFolder(page, 'Term')
  await folderAction(page, 'Term', '+ терминал')
  await expect(page.locator('.tab[data-tab="dev/Term-1"]')).toBeVisible()
  await page.locator(`${shown} .term`).click()
  await page.keyboard.type('echo hi-$((6*7))')
  await page.keyboard.press('Enter')
  await expectTerm(page, 'dev/Term-1', 'hi-42')
  // Вторая сессия и «Открыть рядом»: две группы, обе панели на экране.
  await folderAction(page, 'Term', '+ терминал')
  await expect(page.locator('.tab[data-tab="dev/Term-2"]')).toBeVisible()
  await page.getByRole('button', { name: 'Открыть рядом' }).click()
  await expect(page.locator('.group')).toHaveCount(2)
  await expect(page.locator(shown)).toHaveCount(2)
  await page.screenshot({ path: 'test-results/desktop-split.png' })
  await page.getByRole('button', { name: 'Одна группа' }).click()
  await expect(page.locator('.group')).toHaveCount(1)
  await expect(page.locator(shown)).toHaveCount(1)

  // Без поля ввода загрузка вставляет путь прямо в терминал, Enter не жмёт.
  await page.locator('.tab[data-tab="dev/Term-2"]').click()
  await expectTerm(page, 'dev/Term-2', /\$ *$/m)
  await page.locator(`${shown} .sess-head input[type=file]`).setInputFiles({ name: 'up.txt', mimeType: 'text/plain', buffer: Buffer.from('x') })
  await expectTerm(page, 'dev/Term-2', /\$ up\.txt *$/m)
  await page.locator(`${shown} .term`).click()
  await page.keyboard.press('Control+U')

  // Завершившийся процесс: «Продолжить».
  await page.keyboard.type('exit')
  await page.keyboard.press('Enter')
  await expect(page.getByRole('button', { name: 'Продолжить' })).toBeVisible({ timeout: 15_000 })
  await page.getByRole('button', { name: 'Продолжить' }).click()
  await expect(page.getByRole('button', { name: 'Продолжить' })).toBeHidden({ timeout: 15_000 })
})

test('правка файла: сохранение, правка агента во время правки, сравнение', async ({ page }) => {
  const problems: string[] = []
  page.on('pageerror', (e) => problems.push('pageerror: ' + e.message))
  await page.goto(URL)
  await newFolder(page, 'Ed')
  const file = join(agentHome(7700), 'Ed', 'doc.md')
  writeFileSync(file, '# Док\n\nпервая строка\n')
  await page.locator('.tree .entry[data-path="doc.md"]').click()
  await page.locator(`${shown} .file-head .seg button`, { hasText: 'Правка' }).click()
  const cm = page.locator(`${shown} .cm-content`)
  await expect(cm).toContainText('первая строка')

  // Правка → точка «не сохранено», вкладка закрепилась; Ctrl+S пишет на диск.
  await cm.click()
  await page.keyboard.press('Control+End')
  await page.keyboard.type('вторая строка')
  await expect(page.locator('.tab.file.is-dirty[data-file="doc.md"]')).toBeVisible()
  await expect(page.locator('.tab.file.preview')).toHaveCount(0)
  await page.keyboard.press('Control+s')
  await expect(page.locator('.tab.file.is-dirty')).toHaveCount(0)
  await expect.poll(() => readFileSync(file, 'utf8')).toBe('# Док\n\nпервая строка\nвторая строка')

  // Без правок файл, изменённый агентом, просто подтягивается.
  writeFileSync(file, '# Док агента\n')
  await expect(cm).toContainText('Док агента')

  // Правка, а агент меняет файл: полоса конфликта, сравнение, «Сохранить моё».
  await cm.click()
  await page.keyboard.press('Control+End')
  await page.keyboard.type('моё')
  writeFileSync(file, '# Док агента\nещё от агента\n')
  await expect(page.locator(`${shown} .conflict`)).toBeVisible()
  await page.locator(`${shown} .conflict`).getByRole('button', { name: 'Сравнить' }).click()
  await expect(page.locator(`${shown} .cm-deletedChunk`).first()).toContainText('ещё от агента')
  await page.screenshot({ path: 'test-results/desktop-conflict.png' })
  await page.locator(`${shown} .conflict`).getByRole('button', { name: 'Сохранить моё' }).click()
  await expect(page.locator(`${shown} .conflict`)).toHaveCount(0)
  await expect.poll(() => readFileSync(file, 'utf8')).toBe('# Док агента\nмоё')

  // Ещё раз, но «Взять с диска»: правка выброшена, в редакторе версия агента.
  await cm.click()
  await page.keyboard.press('Control+End')
  await page.keyboard.type(' лишнее')
  writeFileSync(file, 'только агент\n')
  await page.locator(`${shown} .conflict`).getByRole('button', { name: 'Взять с диска' }).click()
  await expect(cm).toHaveText('только агент')
  await expect(page.locator('.tab.file.is-dirty')).toHaveCount(0)
  expect(readFileSync(file, 'utf8')).toBe('только агент\n')
  expect(problems).toEqual([])
})

test('чужой Origin и без входа', async ({ request }) => {
  const r = await request.post('http://localhost:7700/api/agents/dev/workdirs', { data: { name: 'x' }, headers: { Origin: 'https://evil.example' } })
  expect(r.status()).toBe(403)
  const r2 = await request.get('http://localhost:7800/api/agents')
  expect(r2.status()).toBe(401)
})
