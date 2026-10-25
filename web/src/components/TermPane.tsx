import { useEffect, useRef, useState } from 'preact/hooks'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { Unicode11Addon } from '@xterm/addon-unicode11'
import { WebLinksAddon } from '@xterm/addon-web-links'
import '@xterm/xterm/css/xterm.css'
import { socket } from '../ws'
import { post, sessPath } from '../api'
import { setState, tabKey, toast, type SessTab } from '../store'
import { onThemeChange, termTheme } from '../theme'

// Отсоединение скрытой панели: tmux при возврате всё равно перерисует экран.
const DETACH_AFTER = 30_000

interface Props {
  tab: SessTab
  visible: boolean
  focused: boolean
  onFocus: () => void
}

// Для e2e: текст терминалов по ключу вкладки.
declare global {
  interface Window {
    __laavuTerms?: Record<string, Terminal>
  }
}

// Сессии, прокрученные вверх: tmux у них в режиме просмотра истории и съест
// ввод как свои команды. Перед вводом клиент выводит панель из этого режима.
const scrolled = new Set<string>()
// Остановка инерции прокрутки по сессии: иначе она вернёт панель в просмотр
// истории уже после выхода из него.
const stoppers = new Map<string, () => void>()
// Прокрутка и выход из истории идут по одной очереди на сессию: tmux получает
// их ровно в том порядке, в каком они случились.
const queues = new Map<string, Promise<unknown>>()

function enqueue(tab: SessTab, f: () => Promise<unknown>): Promise<unknown> {
  const k = tabKey(tab)
  const p = (queues.get(k) || Promise.resolve()).then(f).catch(() => {})
  queues.set(k, p)
  return p
}

export async function leaveHistory(tab: SessTab) {
  const k = tabKey(tab)
  stoppers.get(k)?.()
  if (!scrolled.delete(k)) return queues.get(k)
  await enqueue(tab, () => post(`${sessPath(tab.agent, tab.session)}/keys`, { keys: ['CancelMode'] }))
}

// scrollBy прокручивает сессию на lines строк (больше нуля — к началу истории)
// той же очередью, что свайп и выход из истории.
export function scrollBy(tab: SessTab, lines: number) {
  if (!lines) return
  if (lines > 0) scrolled.add(tabKey(tab))
  enqueue(tab, () => post(`${sessPath(tab.agent, tab.session)}/scroll`, { lines }))
}

// Отчёты мыши и фокуса xterm шлёт тем же потоком; это не ввод пользователя.
const notInput = (d: string) => d.startsWith('\x1b[<') || d.startsWith('\x1b[M') || d === '\x1b[I' || d === '\x1b[O'

// Свайп по терминалу прокручивает историю tmux построчно, с инерцией после
// отпускания. Строки копятся и уходят пачкой раз в 40 мс.
function touchScroll(container: HTMLElement, term: Terminal, tab: SessTab): { off: () => void; stop: () => void } {
  const key = tabKey(tab)
  const cell = () => container.clientHeight / Math.max(1, term.rows) || 16
  let lastY = 0
  let lastT = 0
  let acc = 0
  let v = 0
  let moved = 0
  let raf = 0
  let lines = 0
  let timer = 0
  const flush = () => {
    timer = 0
    const n = lines
    lines = 0
    scrollBy(tab, n)
  }
  const apply = (dy: number) => {
    acc += dy
    const c = cell()
    const n = Math.trunc(acc / c) // палец вниз — к началу истории
    if (!n) return
    acc -= n * c
    lines += n
    if (!timer) timer = window.setTimeout(flush, 40)
  }
  const start = (e: TouchEvent) => {
    cancelAnimationFrame(raf)
    if (e.touches.length !== 1) return
    lastY = e.touches[0].clientY
    lastT = e.timeStamp
    acc = 0
    v = 0
    moved = 0
  }
  const move = (e: TouchEvent) => {
    if (e.touches.length !== 1) return
    const y = e.touches[0].clientY
    const dy = y - lastY
    const dt = Math.max(1, e.timeStamp - lastT)
    lastY = y
    lastT = e.timeStamp
    moved += Math.abs(dy)
    if (moved < 8) return
    e.preventDefault()
    v = 0.8 * (dy / dt) + 0.2 * v
    apply(dy)
  }
  const end = () => {
    if (moved < 8 || Math.abs(v) < 0.3) return
    let t0 = performance.now()
    const tick = (now: number) => {
      const dt = now - t0
      t0 = now
      apply(v * dt)
      v *= Math.pow(0.95, dt / 16)
      if (Math.abs(v) > 0.05) raf = requestAnimationFrame(tick)
    }
    raf = requestAnimationFrame(tick)
  }
  container.addEventListener('touchstart', start, { passive: true })
  container.addEventListener('touchmove', move, { passive: false })
  container.addEventListener('touchend', end, { passive: true })
  const stop = () => {
    cancelAnimationFrame(raf)
    v = 0
    if (timer) {
      clearTimeout(timer)
      flush()
    }
  }
  return {
    stop,
    off: () => {
      stop()
      container.removeEventListener('touchstart', start)
      container.removeEventListener('touchmove', move)
      container.removeEventListener('touchend', end)
    },
  }
}

export function TermPane({ tab, visible, focused, onFocus }: Props) {
  const el = useRef<HTMLDivElement>(null)
  const st = useRef<{ term: Terminal; fit: FitAddon; ch: number; timer: number; webgl: any; opened: boolean; claim: (force?: unknown) => void } | null>(null)
  const shown = useRef(visible)
  shown.current = visible
  // elsewhere — окно tmux дольше секунды не нашего размера: сессию сейчас
  // ведут с другого устройства, вместо сжатого экрана — плашка.
  const [elsewhere, setElsewhere] = useState(false)
  const win = useRef({ cols: 0, rows: 0, timer: 0 })

  useEffect(() => {
    const term = new Terminal({
      fontFamily: '"JetBrains Mono", ui-monospace, monospace',
      fontSize: 13,
      allowProposedApi: true,
      scrollback: 1000,
      theme: termTheme(),
      lineHeight: 1.15,
      macOptionIsMeta: true,
      cursorBlink: false,
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.loadAddon(new Unicode11Addon())
    term.unicode.activeVersion = '11'
    term.loadAddon(new WebLinksAddon((_, uri) => window.open(uri, '_blank', 'noopener')))
    const s = { term, fit, ch: 0, timer: 0, webgl: null as any, opened: false, claim: (_?: unknown) => {} }
    st.current = s
    const key = tabKey(tab)
    let offTouch = () => {}
    // xterm нельзя открывать в скрытом контейнере: размер символа измерится нулём,
    // и терминал останется пустым. Открываем, когда у элемента появился размер.
    const openNow = () => {
      if (s.opened || !el.current || el.current.offsetWidth === 0) return
      s.opened = true
      term.open(el.current)
      term.textarea?.addEventListener('focus', onFocus)
      term.attachCustomWheelEventHandler((e) => {
        if (e.deltaY < 0) scrolled.add(key)
        return true
      })
      const ts = touchScroll(el.current, term, tab)
      stoppers.set(key, ts.stop)
      offTouch = () => {
        ts.off()
        stoppers.delete(key)
      }
      // WebGL — быстрее; если контекст не получить, остаётся DOM-рендер. На сенсорных
      // устройствах WebGL не включаем: при дробном devicePixelRatio (2.625 у Pixel)
      // холст рисовался пустым, а DOM-рендеру маленького экрана хватает.
      if (window.matchMedia('(pointer: coarse)').matches) return
      import('@xterm/addon-webgl')
        .then(({ WebglAddon }) => {
          try {
            const w = new WebglAddon()
            w.onContextLoss(() => w.dispose())
            term.loadAddon(w)
            s.webgl = w
          } catch {}
        })
        .catch(() => {})
    }
    // Ввод после прокрутки вверх: сначала выход из просмотра истории, потом данные.
    let pending: string[] | null = null
    term.onData((d) => {
      if (!s.ch) return
      if (pending) return void pending.push(d)
      if (notInput(d) || !scrolled.has(key)) return socket.send(s.ch, d)
      pending = [d]
      leaveHistory(tab).then(() => {
        const p = pending || []
        pending = null
        for (const x of p) if (s.ch) socket.send(s.ch, x)
      })
    })
    term.onBinary((d) => s.ch && socket.send(s.ch, d))
    term.onResize(({ cols, rows }) => {
      if (s.ch) socket.resize(s.ch, cols, rows)
      checkSize()
    })
    // У окна tmux один размер на всех клиентов, и он следует за последним активным.
    // Сессия открыта и на телефоне — окно сжимается под него. Вернулись к браузеру
    // (вкладка, окно, касание терминала) — resize того же размера забирает окно назад.
    let lastClaim = 0
    const claim = (force?: unknown) => {
      if (!s.ch || !shown.current || document.visibilityState !== 'visible' || (force !== true && Date.now() - lastClaim < 500)) return
      lastClaim = Date.now()
      socket.resize(s.ch, term.cols, term.rows)
    }
    s.claim = claim
    const onVis = () => document.visibilityState === 'visible' && claim()
    document.addEventListener('visibilitychange', onVis)
    window.addEventListener('focus', claim)
    el.current!.addEventListener('pointerdown', claim, true)
    el.current!.addEventListener('wheel', claim, { capture: true, passive: true })
    el.current!.addEventListener('touchstart', claim, { capture: true, passive: true })
    const offClaim = () => {
      document.removeEventListener('visibilitychange', onVis)
      window.removeEventListener('focus', claim)
    }
    const offTheme = onThemeChange(() => (term.options.theme = termTheme()))
    const ro = new ResizeObserver(() => {
      if (el.current && el.current.offsetWidth > 0) {
        openNow()
        try {
          fit.fit()
        } catch {}
      }
    })
    ro.observe(el.current!)
    window.__laavuTerms = window.__laavuTerms || {}
    window.__laavuTerms[tabKey(tab)] = term
    return () => {
      ro.disconnect()
      offClaim()
      offTheme()
      offTouch()
      clearTimeout(s.timer)
      if (s.ch) socket.close(s.ch)
      delete window.__laavuTerms?.[tabKey(tab)]
      term.dispose()
    }
  }, [tab.agent, tab.session])

  function checkSize() {
    const w = win.current
    const t = st.current?.term
    const differs = !!t && w.cols > 0 && (w.cols !== t.cols || w.rows !== t.rows)
    clearTimeout(w.timer)
    if (!differs) return setElsewhere(false)
    // Своё изменение размера доходит до tmux не мгновенно — ждём секунду.
    w.timer = window.setTimeout(() => setElsewhere(true), 1000)
  }

  // Подключение видимой панели и отсоединение скрытой через 30 с.
  useEffect(() => {
    const s = st.current
    if (!s) return
    if (visible) {
      clearTimeout(s.timer)
      s.timer = 0
      if (s.ch) s.claim()
      if (!s.ch) {
        try {
          s.fit.fit()
        } catch {}
        s.term.reset()
        s.ch = socket.open(tab.agent, tab.session, s.term.cols, s.term.rows, {
          onData: (d) => s.term.write(d),
          onWinsize: (cols, rows) => {
            win.current.cols = cols
            win.current.rows = rows
            checkSize()
          },
          onClosed: (err) => {
            s.ch = 0
            win.current.cols = 0
            checkSize()
            if (err) {
              s.term.write(`\r\n\x1b[2m[${err}]\x1b[0m\r\n`)
              toast(err)
            } else {
              s.term.write('\r\n\x1b[2m[сессия закрыта]\x1b[0m\r\n')
            }
          },
        })
      }
    } else if (s.ch && !s.timer) {
      s.timer = window.setTimeout(() => {
        if (s.ch) socket.close(s.ch)
        s.ch = 0
        s.timer = 0
      }, DETACH_AFTER)
    }
  }, [visible])

  useEffect(() => {
    if (focused && visible && !elsewhere) st.current?.term.focus()
  }, [focused, visible, elsewhere])

  // Спрятанный терминал не должен принимать ввод вслепую.
  useEffect(() => {
    if (elsewhere) st.current?.term.blur()
  }, [elsewhere])

  return (
    <div class={'term-box' + (elsewhere ? ' is-elsewhere' : '')}>
      <div class="term" ref={el} onMouseDown={onFocus} data-session={tab.session} />
      {elsewhere && (
        <div class="term-elsewhere">
          <b>Сессия открыта на другом устройстве</b>
          <span class="muted small">Окно tmux одно на всех и сейчас подстроено под него.</span>
          <button
            class="primary"
            onClick={() => {
              st.current?.claim(true)
              st.current?.term.focus()
            }}
          >
            Вернуть сюда
          </button>
        </div>
      )}
    </div>
  )
}

// Отправить в терминал последовательность (полоса клавиш на телефоне).
export function termOf(tab: SessTab): Terminal | undefined {
  return window.__laavuTerms?.[tabKey(tab)]
}

export function focusTerm(tab: SessTab) {
  termOf(tab)?.focus()
  setState({})
}
