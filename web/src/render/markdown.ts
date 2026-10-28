// Markdown rendering: KaTeX, highlighting, a source line map. Raw HTML passes
// only through safeHtml because Markdown renders on the app's origin. The module loads lazily.
import MarkdownIt from 'markdown-it'
import katexPlugin from '@vscode/markdown-it-katex'
import 'katex/dist/katex.min.css'
import hljs from 'highlight.js/lib/common'
import { safeHtml } from './html'

const md = new MarkdownIt({
  html: true,
  linkify: true,
  typographer: false,
  highlight(code, lang) {
    if (lang && hljs.getLanguage(lang)) {
      try {
        return hljs.highlight(code, { language: lang, ignoreIllegals: true }).value
      } catch {}
    }
    return ''
  },
})
md.use((katexPlugin as any).default || katexPlugin, { throwOnError: false })
md.renderer.rules.html_block = (tokens, idx) => safeHtml(tokens[idx].content)
md.renderer.rules.html_inline = (tokens, idx) => safeHtml(tokens[idx].content)

// Each block carries its source line number, so a selection goes to the session as file:line.
md.core.ruler.push('source_line', (state) => {
  for (const t of state.tokens) {
    if (t.map && t.nesting >= 0 && t.type !== 'inline') t.attrSet('data-source-line', String(t.map[0] + 1))
  }
})

// Heading anchors as GitHub makes them: lower case, no punctuation, spaces to hyphens, -1, -2 for repeats.
export function slug(text: string): string {
  return text
    .trim()
    .toLowerCase()
    .replace(/[^\p{L}\p{N}\p{M} _-]/gu, '')
    .replace(/ /g, '-')
}

md.core.ruler.push('heading_ids', (state) => {
  const used = new Map<string, number>()
  const ts = state.tokens
  for (let i = 0; i < ts.length; i++) {
    if (ts[i].type !== 'heading_open') continue
    const text = (ts[i + 1]?.children || []).filter((c) => c.type === 'text' || c.type === 'code_inline').map((c) => c.content).join('')
    const base = slug(text)
    const n = used.get(base) || 0
    used.set(base, n + 1)
    ts[i].attrSet('id', n ? `${base}-${n}` : base)
  }
})

const decode = (s: string) => {
  try {
    return decodeURIComponent(s)
  } catch {
    return s
  }
}

const isExternal = (u: string) => /^[a-z][a-z0-9+.-]*:/i.test(u) || u.startsWith('//')

function resolve(dir: string, rel: string): string {
  const parts = (rel.startsWith('/') ? [] : dir ? dir.split('/') : []).concat(rel.split('/'))
  const out: string[] = []
  for (const p of parts) {
    if (!p || p === '.') continue
    if (p === '..') out.pop()
    else out.push(p)
  }
  return out.join('/')
}

const defaultFence = md.renderer.rules.fence!
md.renderer.rules.fence = (tokens, idx, opts, env, self) => {
  const t = tokens[idx]
  if (t.info.trim() === 'mermaid') {
    const line = t.map ? t.map[0] + 1 : 0
    return `<div class="mermaid" data-source-line="${line}">${md.utils.escapeHtml(t.content)}</div>`
  }
  return defaultFence(tokens, idx, opts, env, self)
}

md.renderer.rules.image = (tokens, idx, _opts, env: any) => {
  const t = tokens[idx]
  let src = String(t.attrGet('src') || '')
  if (!isExternal(src) && !src.startsWith('data:')) src = env.viewBase + resolve(env.dir, src.split('#')[0]).split('/').map(encodeURIComponent).join('/')
  const alt = md.utils.escapeHtml(t.content)
  return `<img src="${md.utils.escapeHtml(src)}" alt="${alt}" loading="lazy">`
}

md.renderer.rules.link_open = (tokens, idx, opts, env: any, self) => {
  const t = tokens[idx]
  const href = String(t.attrGet('href') || '')
  if (isExternal(href)) {
    t.attrSet('target', '_blank')
    t.attrSet('rel', 'noopener noreferrer')
  } else {
    // A link inside the folder: a file and/or an anchor. The click is handled by the viewer,
    // the address bar of the app stays as it is.
    const i = href.indexOf('#')
    const file = i < 0 ? href : href.slice(0, i)
    const anchor = i < 0 ? '' : href.slice(i + 1)
    if (file) t.attrSet('data-path', resolve(env.dir, decode(file)))
    if (anchor) t.attrSet('data-anchor', decode(anchor))
    t.attrSet('href', '#')
  }
  return self.renderToken(tokens, idx, opts)
}

export function render(src: string, dir: string, viewBase: string): string {
  return md.render(src, { dir, viewBase })
}

let mermaidReady: Promise<any> | null = null

export async function runMermaid(root: HTMLElement) {
  const nodes = root.querySelectorAll<HTMLElement>('.mermaid')
  if (!nodes.length) return
  if (!mermaidReady) {
    mermaidReady = import('mermaid').then((m) => {
      const dark = window.matchMedia('(prefers-color-scheme: dark)').matches
      m.default.initialize({ startOnLoad: false, securityLevel: 'strict', theme: dark ? 'dark' : 'default' })
      return m.default
    })
  }
  const mermaid = await mermaidReady
  try {
    await mermaid.run({ nodes: Array.from(nodes) })
  } catch {}
}
