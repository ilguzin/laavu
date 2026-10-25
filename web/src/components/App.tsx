import { useEffect, useRef, useState } from 'preact/hooks'
import { ApiError, get, lsGet, lsSet, type Agent, type Me } from '../api'
import { Icon } from '../icons'
import { activeTab, folderOf, getState, openTab, setState, toast, useMobile, useStore } from '../store'
import { socket } from '../ws'
import { Login, Register } from './Auth'
import { uploadTo } from './KeyBar'
import { Modals } from './Modals'
import { Navigator } from './Navigator'
import { Rail } from './Rail'
import { refreshWorkdirs, Sidebar } from './Sidebar'
import { Workspace } from './Workspace'

export function App() {
  const [phase, setPhase] = useState<'loading' | 'login' | 'register' | 'app'>(location.pathname === '/register' ? 'register' : 'loading')

  async function boot() {
    try {
      const me = await get<Me>('/api/me')
      const agents = await get<Agent[]>('/api/agents')
      const cur = getState().agent
      setState({ me, agents, agent: agents.some((a) => a.name === cur) ? cur : agents[0]?.name || '' })
      setPhase('app')
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) setPhase('login')
      else {
        toast((e as Error).message)
        setTimeout(boot, 3000)
      }
    }
  }

  useEffect(() => {
    if (phase === 'loading') boot()
  }, [phase])

  if (phase === 'register') return <Register onDone={() => setPhase('loading')} />
  if (phase === 'login') return <Login onDone={() => setPhase('loading')} />
  if (phase === 'loading')
    return (
      <div class="auth muted">
        <span class="logo">
          <Icon name="logo" size={56} stroke={1.6} />
        </span>
      </div>
    )
  return <Main />
}

// Ширина боковой панели, перетаскиваемая за край; запоминается на устройстве.
function useWidth(key: string, def: number, min: number, max: number): [number, (e: PointerEvent, dir: 1 | -1) => void] {
  const [w, setW] = useState(() => Math.min(max, Math.max(min, parseInt(lsGet(key) || '', 10) || def)))
  function start(e: PointerEvent, dir: 1 | -1) {
    e.preventDefault()
    const x0 = e.clientX
    const w0 = w
    let last = w0
    const move = (ev: PointerEvent) => {
      last = Math.min(max, Math.max(min, w0 + dir * (ev.clientX - x0)))
      setW(last)
    }
    const up = () => {
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', up)
      lsSet(key, String(last))
    }
    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', up)
  }
  return [w, start]
}

function Main() {
  const agents = useStore((s) => s.agents)
  const sideOpen = useStore((s) => s.sideOpen)
  const navOpen = useStore((s) => s.navOpen)
  const screen = useStore((s) => s.mobileScreen)
  const toastMsg = useStore((s) => s.toast)
  const sessions = useStore((s) => s.sessions)
  const groups = useStore((s) => s.groups)
  const focus = useStore((s) => s.focus)
  const mobile = useMobile()
  const [sideW, startSide] = useWidth('laavu.sideW', 290, 220, 460)
  const [navW, startNav] = useWidth('laavu.navW', 300, 220, 560)
  const touch = useRef<{ x: number; y: number } | null>(null)

  useEffect(() => {
    socket.onState = (c) => setState({ connected: c })
    const off = socket.on((m) => {
      switch (m.t) {
        case 'sessions':
          setState((s) => ({ sessions: { ...s.sessions, [m.agent]: m.list || [] } }))
          break
        case 'status':
          setState((s) => ({
            sessions: { ...s.sessions, [m.agent]: (s.sessions[m.agent] || []).map((x) => (x.name === m.session ? { ...x, status: m.status } : x)) },
          }))
          // Ход закончился — у папок могли поменяться ветка и изменения.
          if (m.status === 'waiting') refreshWorkdirs(m.agent)
          break
        case 'action':
          setState((s) => ({
            sessions: { ...s.sessions, [m.agent]: (s.sessions[m.agent] || []).map((x) => (x.name === m.session ? { ...x, action: m.action || undefined } : x)) },
          }))
          break
        case 'context':
          setState((s) => ({ context: { ...s.context, [`${m.agent}/${m.session}`]: m.context } }))
          break
      }
    })
    socket.start()
    // Переход из уведомления: /#/агент/сессия.
    const fromHash = () => {
      const m = location.hash.match(/^#\/([^/]+)\/(.+)$/)
      if (m) {
        openTab({ kind: 'session', agent: decodeURIComponent(m[1]), session: decodeURIComponent(m[2]) })
        history.replaceState(null, '', '/')
      }
    }
    fromHash()
    window.addEventListener('hashchange', fromHash)
    navigator.serviceWorker?.addEventListener('message', (e) => {
      if (e.data?.type === 'open') openTab({ kind: 'session', agent: e.data.agent, session: e.data.session })
    })
    // Несохранённые правки — предупреждение при уходе со страницы.
    const beforeUnload = (e: BeforeUnloadEvent) => {
      if (Object.values(getState().dirty).some(Boolean)) e.preventDefault()
    }
    window.addEventListener('beforeunload', beforeUnload)
    const onFocus = () => getState().agent && refreshWorkdirs(getState().agent)
    window.addEventListener('focus', onFocus)
    return () => {
      off()
      socket.stop()
      window.removeEventListener('beforeunload', beforeUnload)
      window.removeEventListener('focus', onFocus)
    }
  }, [])

  useEffect(() => {
    socket.subscribe(agents.map((a) => a.name))
  }, [agents])

  // Навигатор следует за активной вкладкой (папка сессии появляется со списком сессий).
  const cur = activeTab()
  const curFolder = folderOf(cur)
  useEffect(() => {
    if (cur?.kind === 'session') setState({ lastSession: cur })
    if (curFolder && (curFolder.agent !== getState().nav.agent || curFolder.folder !== getState().nav.folder)) setState({ nav: curFolder })
  }, [cur && cur.agent, cur && (cur.kind === 'session' ? cur.session : cur.path), curFolder?.folder])

  async function dropOnWindow(e: DragEvent) {
    const files = e.dataTransfer?.files
    const s = getState().lastSession
    const f = s && folderOf(s)
    if (!files?.length || !s || !f) return
    e.preventDefault()
    uploadTo(s, f.folder, files)
  }

  const waitingAll = Object.values(sessions)
    .flat()
    .filter((s) => s.status === 'waiting').length
  const g = groups[focus]

  return (
    <div
      class={`app screen-${screen}` + (sideOpen ? '' : ' no-side') + (navOpen ? '' : ' no-nav')}
      style={{ '--side-w': `${sideW}px`, '--nav-w': `${navW}px` }}
      onDragOver={(e) => e.dataTransfer?.types.includes('Files') && e.preventDefault()}
      onDrop={dropOnWindow}
      onTouchStart={(e) => (touch.current = { x: e.touches[0].clientX, y: e.touches[0].clientY })}
      onTouchEnd={(e) => {
        // Свайп между вкладками группы на экране вкладки.
        const t0 = touch.current
        touch.current = null
        if (!t0 || !mobile || screen !== 'work' || !g) return
        const dx = e.changedTouches[0].clientX - t0.x
        const dy = e.changedTouches[0].clientY - t0.y
        if (Math.abs(dx) > 80 && Math.abs(dx) > 2 * Math.abs(dy)) {
          const n = g.active + (dx < 0 ? 1 : -1)
          if (n >= 0 && n < g.tabs.length) setState((s) => ({ groups: s.groups.map((x, j) => (j === focus ? { ...x, active: n } : x)) }))
        }
      }}
    >
      <Rail />
      <Sidebar />
      <Workspace />
      <Navigator />
      {!mobile && sideOpen && <div class="resizer" style={{ left: `${60 + sideW - 3}px` }} onPointerDown={(e) => startSide(e, 1)} />}
      {!mobile && navOpen && <div class="resizer" style={{ right: `${navW - 3}px` }} onPointerDown={(e) => startNav(e, -1)} />}
      <nav class="mobile-nav">
        <button class={screen === 'sessions' ? 'on' : ''} onClick={() => setState({ mobileScreen: 'sessions' })}>
          <Icon name="agent" size={21} />
          Сессии
          {waitingAll > 0 && <span class="badge">{waitingAll}</span>}
        </button>
        <button class={screen === 'work' ? 'on' : ''} onClick={() => setState({ mobileScreen: 'work' })} disabled={!g?.tabs.length}>
          <Icon name="terminal" size={21} />
          Открыто
        </button>
        <button class={screen === 'files' ? 'on' : ''} onClick={() => setState({ mobileScreen: 'files', navTab: 'files' })}>
          <Icon name="folder" size={21} />
          Файлы
        </button>
        <button class={screen === 'changes' ? 'on' : ''} onClick={() => setState({ mobileScreen: 'changes', navTab: 'changes' })}>
          <Icon name="git" size={21} />
          Изменения
        </button>
        <button onClick={() => setState({ modal: { kind: 'menu' } })}>
          <Icon name="user" size={21} />
          Ещё
        </button>
      </nav>
      <Modals />
      {toastMsg && <div class="toast">{toastMsg}</div>}
    </div>
  )
}
