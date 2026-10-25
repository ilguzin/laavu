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
  version?: string // есть, если файл можно править (текст не больше предела)
  content?: string
  truncated?: boolean
}

// Origin просмотра: токен на папку, выдаётся демоном и продлевается повторным запросом.
const viewBases = new Map<string, Promise<string>>()
export function viewBase(agent: string, folder: string): Promise<string> {
  const k = `${agent}/${folder}`
  let p = viewBases.get(k)
  if (!p) {
    p = post<{ base: string }>(`${filesPath(agent, folder)}/view`).then((r) => r.base)
    viewBases.set(k, p)
    // Токен живёт час; обновляем заранее.
    setTimeout(() => viewBases.delete(k), 45 * 60 * 1000)
    p.catch(() => viewBases.delete(k))
  }
  return p
}

export const hasView = (kind: string) => kind === 'markdown' || kind === 'html' || kind === 'svg'
export const encPath = (p: string) => p.split('/').map(encodeURIComponent).join('/')

export function fmtSize(n: number) {
  if (n < 1024) return `${n} Б`
  if (n < 1 << 20) return `${(n / 1024).toFixed(1)} КБ`
  return `${(n / (1 << 20)).toFixed(1)} МБ`
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

  // Рендер содержимого в DOM.
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
        el.querySelectorAll<HTMLAnchorElement>('a[data-path]').forEach((a) =>
          a.addEventListener('click', (e) => {
            e.preventDefault()
            openTab({ kind: 'file', agent, folder, path: a.dataset.path! })
          }),
        )
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
    if (!tab) return toast('Сначала откройте сессию')
    const ref = `${folder}/${path}` + (sel.line ? `:${sel.line}` : '')
    const quote = sel.text.split('\n').map((l) => '> ' + l).join('\n')
    insertText(tab, `${ref}\n${quote}\n`)
    setSel(null)
    window.getSelection()?.removeAllRanges()
  }

  return (
    <div class="viewer">
      {err && <p class="error pad">{err}</p>}
      {data.truncated && <p class="notice">Показано начало файла ({fmtSize(data.meta.size)}). Целиком — «скачать».</p>}
      {kind === 'binary' && (
        <div class="binary-info">
          <p>
            <b>{path.split('/').pop()}</b>
          </p>
          <p class="muted">
            {fmtSize(data.meta.size)} · {new Date(data.meta.mtime * 1000).toLocaleString('ru')}
          </p>
          <p>Этот тип файла не показывается.</p>
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
          в сессию
        </button>
      )}
    </div>
  )
}
