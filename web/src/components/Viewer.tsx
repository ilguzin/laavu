import { useEffect, useRef, useState } from 'preact/hooks'
import { filesPath, post, type FileMeta } from '../api'
import { getState, openTab, toast } from '../store'
import { Icon } from '../icons'
import { insertText } from './KeyBar'

interface Props {
  agent: string
  folder: string
  path: string
  data: Loaded
  mode: 'view' | 'source'
}

export interface Loaded {
  meta: FileMeta
  version?: string // set if the file is editable (text within the size limit)
  content?: string
  truncated?: boolean
}

// Preview origin: a per-folder token, issued by the daemon and renewed by asking again.
const viewBases = new Map<string, Promise<string>>()
export function viewBase(agent: string, folder: string): Promise<string> {
  const k = `${agent}/${folder}`
  let p = viewBases.get(k)
  if (!p) {
    p = post<{ base: string }>(`${filesPath(agent, folder)}/view`).then((r) => r.base)
    viewBases.set(k, p)
    // The token lives an hour; renew it ahead of time.
    setTimeout(() => viewBases.delete(k), 45 * 60 * 1000)
    p.catch(() => viewBases.delete(k))
  }
  return p
}

export const hasView = (kind: string) => kind === 'markdown' || kind === 'html' || kind === 'svg'
export const encPath = (p: string) => p.split('/').map(encodeURIComponent).join('/')

export function fmtSize(n: number) {
  if (n < 1024) return `${n} B`
  if (n < 1 << 20) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / (1 << 20)).toFixed(1)} MB`
}

// Anchors waiting for a Markdown file to open: a link from another file.
const pendingAnchors = new Map<string, string>()
const anchorKey = (agent: string, folder: string, path: string) => `${agent}/${folder}/${path}`
const isMarkdown = (p: string) => /\.(md|markdown)$/i.test(p)

// scrollToAnchor scrolls the preview to a heading. The tab may be about to show, so it waits a few frames.
function scrollToAnchor(root: HTMLElement, anchor: string, tries = 20) {
  const want = anchor.toLowerCase()
  const target = Array.from(root.querySelectorAll<HTMLElement>('[id]')).find((n) => n.id === anchor) || Array.from(root.querySelectorAll<HTMLElement>('[id]')).find((n) => n.id === want)
  if (!target) return
  if (!target.getClientRects().length) {
    if (tries > 0) requestAnimationFrame(() => scrollToAnchor(root, anchor, tries - 1))
    return
  }
  target.scrollIntoView({ block: 'start' })
}

export function Viewer({ agent, folder, path, data, mode }: Props) {
  const [err, setErr] = useState('')
  const [base, setBase] = useState('')
  const [sel, setSel] = useState<{ line: number; text: string } | null>(null)
  const body = useRef<HTMLDivElement>(null)
  const dir = path.includes('/') ? path.slice(0, path.lastIndexOf('/')) : ''
  const kind = data.meta.kind
  const showView = hasView(kind) && mode === 'view'

  useEffect(() => {
    let cancelled = false
    viewBase(agent, folder)
      .then((b) => !cancelled && setBase(b))
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [agent, folder])

  // Render the content into the DOM.
  useEffect(() => {
    const el = body.current
    if (!el) return
    let cancelled = false
    setErr('')
    el.innerHTML = ''
    const content = data.content ?? ''
    if (kind === 'markdown' && showView) {
      if (!base) return
      import('../render/markdown').then(async (m) => {
        if (cancelled) return
        el.innerHTML = `<div class="md">${m.render(content, dir, base)}</div>`
        el.querySelectorAll<HTMLAnchorElement>('a[data-path], a[data-anchor]').forEach((a) =>
          a.addEventListener('click', (e) => {
            e.preventDefault()
            const to = a.dataset.path
            const anchor = a.dataset.anchor || ''
            if (!to || to === path) return anchor && scrollToAnchor(el, anchor)
            if (anchor && isMarkdown(to)) pendingAnchors.set(anchorKey(agent, folder, to), anchor)
            openTab({ kind: 'file', agent, folder, path: to }, anchor && isMarkdown(to) ? { mode: 'view' } : {})
            // The file may already be open and rendered: it picks the anchor up from the event.
            window.dispatchEvent(new Event('laavu-anchor'))
          }),
        )
        takeAnchor()
        m.runMermaid(el)
      })
    } else if ((kind === 'code' || kind === 'text' || kind === 'markdown' || kind === 'html' || kind === 'svg') && !showView) {
      import('../render/highlight').then((m) => {
        if (cancelled) return
        const lines = m.highlight(content, path)
        const pre = document.createElement('pre')
        pre.className = 'code hljs'
        pre.innerHTML = lines.map((l, i) => `<div class="ln" data-line="${i + 1}"><span class="no">${i + 1}</span><span class="src">${l || ' '}</span></div>`).join('')
        el.appendChild(pre)
      })
    } else if (kind === 'pdf' && base) {
      import('../render/pdf').then((m) => m.renderPdf(base + encPath(path), el, () => cancelled).catch((e) => !cancelled && setErr(String(e?.message || e))))
    }
    return () => {
      cancelled = true
    }
  }, [data, showView, base])

  // takeAnchor scrolls to an anchor left for this file by a link in another one.
  function takeAnchor() {
    const k = anchorKey(agent, folder, path)
    const anchor = pendingAnchors.get(k)
    const el = body.current
    if (!anchor || !el?.querySelector('.md')) return
    pendingAnchors.delete(k)
    scrollToAnchor(el, anchor)
  }

  useEffect(() => {
    window.addEventListener('laavu-anchor', takeAnchor)
    return () => window.removeEventListener('laavu-anchor', takeAnchor)
  }, [agent, folder, path])

  function onMouseUp() {
    const s = window.getSelection()
    const text = s?.toString().trim() || ''
    if (!s || !text || !body.current?.contains(s.anchorNode)) {
      setSel(null)
      return
    }
    let n: Node | null = s.anchorNode
    let line = 0
    while (n && n !== body.current) {
      if (n instanceof HTMLElement) {
        const l = n.dataset.sourceLine || n.dataset.line
        if (l) {
          line = parseInt(l, 10)
          break
        }
      }
      n = n.parentNode
    }
    setSel({ line, text })
  }

  function toSession() {
    if (!sel) return
    const tab = getState().lastSession
    if (!tab) return toast('Open a session first')
    const ref = `${folder}/${path}` + (sel.line ? `:${sel.line}` : '')
    const quote = sel.text.split('\n').map((l) => '> ' + l).join('\n')
    insertText(tab, `${ref}\n${quote}\n`)
    setSel(null)
    window.getSelection()?.removeAllRanges()
  }

  return (
    <div class="viewer">
      {err && <p class="error pad">{err}</p>}
      {data.truncated && <p class="notice">Showing the start of the file ({fmtSize(data.meta.size)}). Download it to see the whole thing.</p>}
      {kind === 'binary' && (
        <div class="binary-info">
          <p>
            <b>{path.split('/').pop()}</b>
          </p>
          <p class="muted">
            {fmtSize(data.meta.size)} · {new Date(data.meta.mtime * 1000).toLocaleString('en')}
          </p>
          <p>This file type cannot be previewed.</p>
        </div>
      )}
      {kind === 'image' && base && (
        <div class="image-view">
          <img src={base + encPath(path)} alt={path} />
        </div>
      )}
      {kind === 'svg' && showView && base && (
        <div class="image-view">
          <img src={base + encPath(path)} alt={path} />
        </div>
      )}
      {kind === 'html' && showView && base && (
        <iframe class="html-view" src={base + encPath(path)} sandbox="allow-scripts allow-forms allow-popups" referrerpolicy="no-referrer" title={path} />
      )}
      <div class="viewer-body" ref={body} onMouseUp={onMouseUp} onTouchEnd={() => setTimeout(onMouseUp, 50)} />
      {sel && (
        <button class="primary sel-action" onMouseDown={(e) => e.preventDefault()} onClick={toSession}>
          <Icon name="quote" size={15} />
          Send to session
        </button>
      )}
    </div>
  )
}
