import { expect, test } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { agentHome, expectTerm, folderAction, newFolder } from './helpers'

const URL = 'http://localhost:7700/'

// Minimal one-page PDF with text.
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

test('agent session: status, terminal input, files in tabs, preview, upload, changes', async ({ page }) => {
  const problems: string[] = []
  page.on('pageerror', (e) => problems.push('pageerror: ' + e.message))
  page.on('console', (m) => {
    if (/Content Security Policy/i.test(m.text()) || (m.type() === 'error' && !/Failed to load resource: .* 422/.test(m.text()))) problems.push(m.text())
  })
  await page.goto(URL)
  await newFolder(page, 'Proj')
  await folderAction(page, 'Proj', '+ session')
  await expect(page.locator('.tab[data-tab="dev/Proj-1"]')).toBeVisible()
  // Stub SessionStart -> "waiting for reply", session in the "Waiting for you" block.
  await expect(page.locator('.sess[data-session="Proj-1"] .dot')).toHaveClass(/st-waiting/, { timeout: 15_000 })
  await expect(page.locator('.wait-card[data-waiting="Proj-1"]')).toBeVisible()
  await expectTerm(page, 'dev/Proj-1', /fakeclaude .* \(startup\)/)

  // No input field: type straight into the terminal.
  await expect(page.locator('textarea:not(.xterm-helper-textarea)')).toHaveCount(0)
  await page.locator(`${shown} .term`).click()
  await page.keyboard.type('grüß dich')
  await page.keyboard.press('Enter')
  await expectTerm(page, 'dev/Proj-1', 'echo: grüß dich')
  // Context indicator after a turn.
  await expect(page.locator('.sess-head .ctx')).toContainText('context')

  // A file written by the agent shows up in the tree via inotify and opens in a tab.
  // A git folder created outside the client: the folder list is reread after a turn.
  const gitDir = join(agentHome(7700), 'Gitty')
  mkdirSync(gitDir, { recursive: true })
  execFileSync('git', ['init', '-q', '-b', 'trunk', gitDir])
  writeFileSync(join(gitDir, 'a.txt'), 'a')
  await page.keyboard.type('write:x.md')
  await page.keyboard.press('Enter')
  await expect(page.locator('.tree .entry[data-path="x.md"]')).toBeVisible()
  // The agent's last action is in the session list; branch and changes are on the folder.
  await expect(page.locator('.sess[data-session="Proj-1"] .act')).toHaveText(' · Write x.md')
  await expect(page.locator('.folder[data-folder="Gitty"] .branch')).toHaveText('trunk')
  await expect(page.locator('.folder[data-folder="Gitty"] .chg-count')).toHaveText('±1')
  await page.locator('.tree .entry[data-path="x.md"]').click()
  await expect(page.locator(`${shown} .viewer .md`)).toContainText('fake')
  await expect(page.locator('.tab.file.preview[data-file="x.md"]')).toBeVisible()

  // Markdown with a formula, mermaid, and html on the preview origin.
  const home = agentHome(7700)
  mkdirSync(join(home, 'Proj', 'docs'), { recursive: true })
  writeFileSync(join(home, 'Proj', 'docs', 'note.md'), '# Heading\n\nFormula $x^2$.\n\n![pic](pic.svg)\n\n```mermaid\ngraph TD; A-->B\n```\n\n<details><summary><b>Hint</b></summary>\n\nHidden $y$.\n\n</details>\n\n<img src=x onerror="document.title=1">\n')
  writeFileSync(join(home, 'Proj', 'docs', 'pic.svg'), '<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10"/></svg>')
  writeFileSync(join(home, 'Proj', 'page.html'), '<!doctype html><body><div id="r"></div><script>document.getElementById("r").textContent="html-" + (6*7)</script></body>')
  await page.locator('.tree .entry[data-path="docs"]').click()
  await page.locator('.tree .entry[data-path="docs/note.md"]').click()
  // The temporary tab was replaced rather than added.
  await expect(page.locator('.tab.file')).toHaveCount(1)
  await expect(page.locator(`${shown} .viewer .md h1`)).toHaveText('Heading')
  await expect(page.locator(`${shown} .viewer .md .katex`).first()).toBeVisible()
  await expect(page.locator(`${shown} .viewer .md img`)).toHaveAttribute('src', /^http:\/\/localhost:7701\/t\/[^/]+\/docs\/pic\.svg$/)
  await expect(page.locator(`${shown} .viewer .md .mermaid svg`)).toBeVisible({ timeout: 20_000 })
  // Raw HTML: details and summary work, other tags stay text.
  await expect(page.locator(`${shown} .viewer .md details summary b`)).toHaveText('Hint')
  await expect(page.locator(`${shown} .viewer .md details .katex`)).toBeHidden()
  await page.locator(`${shown} .viewer .md summary`).click()
  await expect(page.locator(`${shown} .viewer .md details .katex`)).toBeVisible()
  await expect(page.locator(`${shown} .viewer .md img[onerror]`)).toHaveCount(0)
  await expect(page.locator(`${shown} .viewer .md`)).toContainText('<img src=x')
  // Double click pins the tab.
  await page.locator('.tab.file[data-file="docs/note.md"]').dblclick()
  await expect(page.locator('.tab.file.preview')).toHaveCount(0)
  // "Source" mode with line numbers.
  await page.locator(`${shown} .file-head .seg button`, { hasText: 'Source' }).click()
  await expect(page.locator(`${shown} .viewer .ln[data-line="1"]`)).toContainText('# Heading')
  // Select a line -> "Send to session": the quote is pasted into the terminal.
  await page.locator(`${shown} .viewer .ln[data-line="3"] .src`).selectText()
  await page.locator(`${shown} .viewer-body`).dispatchEvent('mouseup')
  await page.getByRole('button', { name: 'Send to session' }).click()
  // The session was not on screen, so its tab became active.
  await expect(page.locator('.tab.on[data-tab="dev/Proj-1"]')).toBeVisible()
  await expectTerm(page, 'dev/Proj-1', /Proj\/docs\/note\.md:3/)
  await page.locator('.tab.file[data-file="docs/note.md"]').click()
  await page.locator(`${shown} .file-head .seg button`, { hasText: 'Preview' }).click()

  // Links with anchors: within the file and into a neighbouring one.
  const filler = Array.from({ length: 80 }, (_, i) => `Paragraph ${i}.`).join('\n\n')
  writeFileSync(join(home, 'Proj', 'docs', 'a.md'), `# A\n\n[to the end](#the-end)\n\n${filler}\n\n## The end\n\n[to b](b.md#second-part)\n`)
  writeFileSync(join(home, 'Proj', 'docs', 'b.md'), `# B\n\n${filler}\n\n## Second part\n\nHere.\n`)
  await page.locator('.tree .entry[data-path="docs/a.md"]').click()
  await page.locator(`${shown} .md a`, { hasText: 'to the end' }).click()
  await expect(page.locator(`${shown} .md #the-end`)).toBeInViewport()
  await page.locator(`${shown} .md a`, { hasText: 'to b' }).click()
  await expect(page.locator('.tab.on[data-file="docs/b.md"]')).toBeVisible()
  await expect(page.locator(`${shown} .md #second-part`)).toBeInViewport()
  expect(page.url()).toBe(URL)

  // A new file opens next to the pinned one.
  await page.locator('.tree .entry[data-path="page.html"]').click()
  await expect(page.locator('.tab.file')).toHaveCount(2)
  const frame = page.frameLocator(`${shown} iframe.html-view`)
  await expect(frame.locator('#r')).toHaveText('html-42')
  await expect(page.locator(`${shown} iframe.html-view`)).toHaveAttribute('src', /^http:\/\/localhost:7701\//)

  // PDF via pdf.js from the preview URL.
  writeFileSync(join(home, 'Proj', 'doc.pdf'), tinyPdf('Laavu PDF'))
  await page.locator('.tree .entry[data-path="doc.pdf"]').click()
  await expect(page.locator(`${shown} .viewer canvas.pdf-page`)).toBeVisible({ timeout: 20_000 })
  await page.screenshot({ path: 'test-results/desktop.png' })

  // Upload via the session header button: the path goes to the terminal.
  await page.locator('.tab[data-tab="dev/Proj-1"]').click()
  await page.locator(`${shown} .sess-head input[type=file]`).setInputFiles({ name: 'photo.jpg', mimeType: 'image/jpeg', buffer: Buffer.from('JPEG') })
  await expectTerm(page, 'dev/Proj-1', 'photo.jpg')
  await expect(page.locator('.tree .entry[data-path="photo.jpg"]')).toBeVisible()

  // What changed (no git, so by time): the navigator tab.
  await folderAction(page, 'Proj', 'What changed')
  await expect(page.getByTestId('changes')).toContainText('x.md')
  await expect(page.getByTestId('changes')).toContainText('photo.jpg')
  await page.getByRole('button', { name: 'Mark as seen' }).click()
  await expect(page.getByTestId('changes')).toContainText('Nothing changed')

  // Pane history.
  await page.getByRole('button', { name: 'History', exact: true }).click()
  await expect(page.locator('.modal-history .xterm')).toBeVisible()
  await page.locator('.modal-close').click()
  await page.screenshot({ path: 'test-results/desktop-session.png' })
  expect(problems, 'console errors and CSP violations').toEqual([])
})

test('terminal: input straight into xterm, side-by-side tabs, paste without Enter', async ({ page }) => {
  await page.goto(URL)
  await newFolder(page, 'Term')
  await folderAction(page, 'Term', '+ terminal')
  await expect(page.locator('.tab[data-tab="dev/Term-1"]')).toBeVisible()
  await page.locator(`${shown} .term`).click()
  await page.keyboard.type('echo hi-$((6*7))')
  await page.keyboard.press('Enter')
  await expectTerm(page, 'dev/Term-1', 'hi-42')
  // A second session and "Open to the side": two groups, both panes on screen.
  await folderAction(page, 'Term', '+ terminal')
  await expect(page.locator('.tab[data-tab="dev/Term-2"]')).toBeVisible()
  await page.getByRole('button', { name: 'Open to the side' }).click()
  await expect(page.locator('.group')).toHaveCount(2)
  await expect(page.locator(shown)).toHaveCount(2)
  await page.screenshot({ path: 'test-results/desktop-split.png' })
  await page.getByRole('button', { name: 'Single group' }).click()
  await expect(page.locator('.group')).toHaveCount(1)
  await expect(page.locator(shown)).toHaveCount(1)

  // With no input field, upload pastes the path straight into the terminal without Enter.
  await page.locator('.tab[data-tab="dev/Term-2"]').click()
  await expectTerm(page, 'dev/Term-2', /\$ *$/m)
  await page.locator(`${shown} .sess-head input[type=file]`).setInputFiles({ name: 'up.txt', mimeType: 'text/plain', buffer: Buffer.from('x') })
  await expectTerm(page, 'dev/Term-2', /\$ up\.txt *$/m)
  await page.locator(`${shown} .term`).click()
  await page.keyboard.press('Control+U')

  // Exited process: "Resume".
  await page.keyboard.type('exit')
  await page.keyboard.press('Enter')
  await expect(page.getByRole('button', { name: 'Resume' })).toBeVisible({ timeout: 15_000 })
  await page.getByRole('button', { name: 'Resume' }).click()
  await expect(page.getByRole('button', { name: 'Resume' })).toBeHidden({ timeout: 15_000 })
})

test('file editing: save, agent edit during an edit, compare', async ({ page }) => {
  const problems: string[] = []
  page.on('pageerror', (e) => problems.push('pageerror: ' + e.message))
  await page.goto(URL)
  await newFolder(page, 'Ed')
  const file = join(agentHome(7700), 'Ed', 'doc.md')
  writeFileSync(file, '# Doc\n\nfirst line\n')
  await page.locator('.tree .entry[data-path="doc.md"]').click()
  await page.locator(`${shown} .file-head .seg button`, { hasText: 'Edit' }).click()
  const cm = page.locator(`${shown} .cm-content`)
  await expect(cm).toContainText('first line')

  // Edit -> "unsaved" dot, the tab got pinned; Ctrl+S writes to disk.
  await cm.click()
  await page.keyboard.press('Control+End')
  await page.keyboard.type('second line')
  await expect(page.locator('.tab.file.is-dirty[data-file="doc.md"]')).toBeVisible()
  await expect(page.locator('.tab.file.preview')).toHaveCount(0)
  await page.keyboard.press('Control+s')
  await expect(page.locator('.tab.file.is-dirty')).toHaveCount(0)
  await expect.poll(() => readFileSync(file, 'utf8')).toBe('# Doc\n\nfirst line\nsecond line')

  // With no edits, a file changed by the agent is simply pulled in.
  writeFileSync(file, '# Agent doc\n')
  await expect(cm).toContainText('Agent doc')

  // Editing while the agent changes the file: conflict bar, compare, "Save mine".
  await cm.click()
  await page.keyboard.press('Control+End')
  await page.keyboard.type('mine')
  writeFileSync(file, '# Agent doc\nmore from agent\n')
  await expect(page.locator(`${shown} .conflict`)).toBeVisible()
  await page.locator(`${shown} .conflict`).getByRole('button', { name: 'Compare' }).click()
  await expect(page.locator(`${shown} .cm-deletedChunk`).first()).toContainText('more from agent')
  await page.screenshot({ path: 'test-results/desktop-conflict.png' })
  await page.locator(`${shown} .conflict`).getByRole('button', { name: 'Save mine' }).click()
  await expect(page.locator(`${shown} .conflict`)).toHaveCount(0)
  await expect.poll(() => readFileSync(file, 'utf8')).toBe('# Agent doc\nmine')

  // Once more, but "Take from disk": the edit is dropped, the editor shows the agent version.
  await cm.click()
  await page.keyboard.press('Control+End')
  await page.keyboard.type(' extra')
  writeFileSync(file, 'agent only\n')
  await page.locator(`${shown} .conflict`).getByRole('button', { name: 'Take from disk' }).click()
  await expect(cm).toHaveText('agent only')
  await expect(page.locator('.tab.file.is-dirty')).toHaveCount(0)
  expect(readFileSync(file, 'utf8')).toBe('agent only\n')
  expect(problems).toEqual([])
})

test('agent settings: ssh key; new session in a git clone', async ({ page }) => {
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(e.message))
  const home = agentHome(7700)
  rmSync(join(home, '.ssh'), { recursive: true, force: true })
  await page.goto(URL)
  await page.getByRole('button', { name: 'Agent settings' }).click()
  await page.getByRole('button', { name: 'Create key' }).click()
  await expect(page.getByLabel('Public key')).toHaveValue(/^ssh-ed25519 \S+ \S+@\S+$/)
  await expect(page.getByRole('button', { name: 'Create key' })).toBeHidden()
  const pub = readFileSync(join(home, '.ssh', 'id_ed25519.pub'), 'utf8').trim()
  await expect(page.getByLabel('Public key')).toHaveValue(pub)
  await page.screenshot({ path: 'test-results/agent-settings.png' })
  await page.getByRole('button', { name: 'close' }).click()

  // A local repository stands in for GitHub: dev mode lets clone take a path.
  const repo = `tool${Date.now() % 1e6}` // a fresh folder and session name on every run
  const src = join(mkdtempSync(join(tmpdir(), 'laavu-e2e-')), `${repo}.git`)
  execFileSync('git', ['init', '-q', '-b', 'trunk', src])
  execFileSync('git', ['-C', src, '-c', 'user.name=t', '-c', 'user.email=t@t', 'commit', '-q', '--allow-empty', '-m', 'first'])

  await page.locator('.side-foot').getByRole('button', { name: 'New session' }).click()
  const modal = page.locator('.modal')
  await modal.getByRole('button', { name: 'Terminal' }).click()
  await modal.getByRole('button', { name: 'Clone from git' }).click()
  await page.getByLabel('Repository address').fill('ext::sh -c true')
  await expect(page.getByLabel('Clone workspace')).toHaveValue('sh--c-true')
  await modal.getByRole('button', { name: 'Start' }).click()
  await expect(page.getByTestId('clone-error')).toContainText('repository address')
  await page.screenshot({ path: 'test-results/new-session-clone.png' })
  await page.getByLabel('Repository address').fill(src)
  await expect(page.getByLabel('Clone workspace')).toHaveValue(repo)
  await modal.getByRole('button', { name: 'Start' }).click()
  await expect(page.locator(`.tab[data-tab="dev/${repo}-1"]`)).toBeVisible()
  await expect(page.locator(`.folder[data-folder="${repo}"] .branch`)).toContainText('trunk')
  await expectTerm(page, `dev/${repo}-1`, /\$ *$/m)
  await page.locator(`${shown} .term`).click()
  await page.keyboard.type('git log --format=%s')
  await page.keyboard.press('Enter')
  await expectTerm(page, `dev/${repo}-1`, /^first$/m)
  // "Changes" in a repository is the working tree against the last commit.
  await page.locator('.nav-head').getByRole('button', { name: 'Changes' }).click()
  await expect(page.getByTestId('changes')).toContainText('Working tree clean')
  writeFileSync(join(home, repo, 'new.txt'), 'hello\n')
  await expect(page.locator('.changes .change[data-path="new.txt"]')).toContainText('U')
  await expect(page.getByRole('button', { name: 'Mark as seen' })).toHaveCount(0)
  await page.locator('.changes .change[data-path="new.txt"]').click()
  await expect(page.locator(`${shown} .diff .add`).last()).toHaveText('+hello')
  expect(errors).toEqual([])
})

test('foreign Origin and no sign-in', async ({ request }) => {
  const r = await request.post('http://localhost:7700/api/agents/dev/workdirs', { data: { name: 'x' }, headers: { Origin: 'https://evil.example' } })
  expect(r.status()).toBe(403)
  const r2 = await request.get('http://localhost:7800/api/agents')
  expect(r2.status()).toBe(401)
})
