// Client state: a small observable store with no libraries.
import { useEffect, useState } from 'preact/hooks'
import type { Agent, ContextUsage, Me, SessionInfo, Workdir } from './api'
import { lsGet, lsSet } from './api'

// A tab is an agent session or a file in a working folder.
export interface SessTab {
  kind: 'session'
  agent: string
  session: string
}

export interface FileTab {
  kind: 'file'
  agent: string
  folder: string
  path: string
  preview?: boolean // temporary: the next opened file replaces it
}

export type Tab = SessTab | FileTab

export type FileMode = 'view' | 'source' | 'edit' | 'diff'

// Tab group: its own tab strip and its own active tab. 1 to 4 groups.
export interface Group {
  tabs: Tab[]
  active: number
}

export const MAX_GROUPS = 4

export interface State {
  me: Me | null
  connected: boolean
  agents: Agent[]
  agent: string // agent shown in the session list
  workdirs: Record<string, Workdir[]>
  sessions: Record<string, SessionInfo[]>
  context: Record<string, ContextUsage>
  groups: Group[]
  focus: number // focused group
  lastSession: SessTab | null // target for "send to session" from a file
  fileModes: Record<string, FileMode>
  dirty: Record<string, boolean> // unsaved edits by tabId
  // Right-hand navigator: folder, tab, hidden files.
  nav: { agent: string; folder: string }
  navTab: 'files' | 'changes'
  showHidden: boolean
  sideOpen: boolean
  navOpen: boolean
  mobileScreen: 'sessions' | 'work' | 'files' | 'changes'
  toast: string
  modal: null | { kind: 'history' | 'device' | 'newSession' | 'agentSettings' | 'menu'; data?: any }
}

const initial: State = {
  me: null,
  connected: false,
  agents: [],
  agent: lsGet('laavu.agent') || '',
  workdirs: {},
  sessions: {},
  context: {},
  groups: loadGroups(),
  focus: 0,
  lastSession: null,
  fileModes: {},
  dirty: {},
  nav: { agent: '', folder: '' },
  navTab: 'files',
  showHidden: false,
  sideOpen: lsGet('laavu.side') !== '0',
  navOpen: lsGet('laavu.nav') !== '0',
  mobileScreen: 'sessions',
  toast: '',
  modal: null,
}

function validTab(t: any): t is Tab {
  if (!t || !t.agent) return false
  if (t.kind === 'file') return typeof t.folder === 'string' && typeof t.path === 'string' && !!t.path
  return !!t.session
}

function loadGroups(): Group[] {
  try {
    const v = JSON.parse(lsGet('laavu.groups') || 'null')
    if (Array.isArray(v)) {
      const gs = v
        .map((g: any) => ({ tabs: Array.isArray(g?.tabs) ? g.tabs.filter(validTab) : [], active: Number(g?.active) || 0 }))
        .filter((g: Group) => g.tabs.length)
        .slice(0, MAX_GROUPS)
      for (const g of gs) g.active = Math.min(g.active, g.tabs.length - 1)
      if (gs.length) return gs
    }
    // Pass 1 format: a list of session tabs.
    const old = JSON.parse(lsGet('laavu.tabs') || '[]')
    if (Array.isArray(old)) {
      const tabs = old.filter((t: any) => t && t.agent && t.session).map((t: any) => ({ kind: 'session' as const, agent: t.agent, session: t.session }))
      if (tabs.length) return [{ tabs, active: 0 }]
    }
  } catch {}
  return [{ tabs: [], active: 0 }]
}

let state: State = initial
const subs = new Set<() => void>()

export function getState(): State {
  return state
}

export function setState(patch: Partial<State> | ((s: State) => Partial<State>)) {
  const p = typeof patch === 'function' ? patch(state) : patch
  state = { ...state, ...p }
  if ('groups' in p) lsSet('laavu.groups', JSON.stringify(state.groups))
  if ('agent' in p) lsSet('laavu.agent', state.agent)
  if ('sideOpen' in p) lsSet('laavu.side', state.sideOpen ? '1' : '0')
  if ('navOpen' in p) lsSet('laavu.nav', state.navOpen ? '1' : '0')
  for (const s of subs) s()
}

export function useStore<T>(sel: (s: State) => T): T {
  const [, force] = useState(0)
  const v = sel(state)
  useEffect(() => {
    let last = sel(state)
    const f = () => {
      const n = sel(state)
      if (n !== last) {
        last = n
        force((x) => x + 1)
      }
    }
    subs.add(f)
    return () => {
      subs.delete(f)
    }
  }, [])
  return v
}

// Session key: drafts, focus in the daemon, terminals for e2e.
export const tabKey = (t: { agent: string; session: string }) => `${t.agent}/${t.session}`

export function tabId(t: Tab): string {
  return t.kind === 'file' ? `f:${t.agent}/${t.folder}/${t.path}` : `s:${tabKey(t)}`
}

export function toast(msg: string) {
  setState({ toast: msg })
  setTimeout(() => {
    if (state.toast === msg) setState({ toast: '' })
  }, 4000)
}

export function sessionOf(t: Tab | undefined): SessionInfo | undefined {
  if (!t || t.kind !== 'session') return undefined
  return state.sessions[t.agent]?.find((s) => s.name === t.session)
}

export function activeTab(s: State = state): Tab | undefined {
  const g = s.groups[s.focus]
  return g?.tabs[g.active]
}

// A tab's folder: the working folder for a session, its own folder for a file.
export function folderOf(t: Tab | undefined): { agent: string; folder: string } | null {
  if (!t) return null
  if (t.kind === 'file') return { agent: t.agent, folder: t.folder }
  const s = sessionOf(t)
  return s?.folder ? { agent: t.agent, folder: s.folder } : null
}

function findTab(gs: Group[], id: string): [number, number] {
  for (let g = 0; g < gs.length; g++) {
    const i = gs[g].tabs.findIndex((t) => tabId(t) === id)
    if (i >= 0) return [g, i]
  }
  return [-1, -1]
}

// Make a tab active in its group and focus that group.
function activate(s: State, g: number, i: number): Partial<State> {
  const groups = s.groups.map((x, j) => (j === g ? { ...x, active: i } : x))
  const t = groups[g].tabs[i]
  const out: Partial<State> = { groups, focus: g, mobileScreen: 'work' }
  if (t.kind === 'session') out.lastSession = t
  const f = folderOf(t)
  if (f) out.nav = f
  return out
}

export function focusTab(g: number, i: number) {
  setState((s) => (s.groups[g]?.tabs[i] ? activate(s, g, i) : {}))
}

// openTab opens a tab or switches to an already open one. A file without pin opens
// as a temporary tab and replaces the previous temporary one in the same group.
export function openTab(t: Tab, opts: { pin?: boolean; mode?: FileMode; group?: number } = {}) {
  setState((s) => {
    const id = tabId(t)
    const modes = opts.mode ? { ...s.fileModes, [id]: opts.mode } : s.fileModes
    const [fg, fi] = findTab(s.groups, id)
    if (fg >= 0) {
      let groups = s.groups
      if (opts.pin) {
        groups = groups.map((g, j) => (j === fg ? { ...g, tabs: g.tabs.map((x, k) => (k === fi && x.kind === 'file' ? { ...x, preview: false } : x)) } : g))
      }
      return { ...activate({ ...s, groups }, fg, fi), fileModes: modes }
    }
    const g = Math.min(opts.group ?? s.focus, s.groups.length - 1)
    const tab: Tab = t.kind === 'file' ? { ...t, preview: !opts.pin } : t
    const tabs = [...s.groups[g].tabs]
    let at = tabs.length
    if (tab.kind === 'file' && tab.preview) {
      const p = tabs.findIndex((x) => x.kind === 'file' && x.preview && !s.dirty[tabId(x)])
      if (p >= 0) at = p
    }
    tabs.splice(at, at < tabs.length ? 1 : 0, tab)
    const groups = s.groups.map((x, j) => (j === g ? { ...x, tabs } : x))
    return { ...activate({ ...s, groups }, g, at), fileModes: modes }
  })
}

// Pin a temporary tab (double click, first edit).
export function pinTab(id: string) {
  setState((s) => ({
    groups: s.groups.map((g) => ({ ...g, tabs: g.tabs.map((t) => (tabId(t) === id && t.kind === 'file' && t.preview ? { ...t, preview: false } : t)) })),
  }))
}

export function closeTab(g: number, i: number) {
  setState((s) => {
    const gr = s.groups[g]
    if (!gr) return {}
    const id = tabId(gr.tabs[i])
    const tabs = gr.tabs.filter((_, j) => j !== i)
    let groups = s.groups.map((x, j) => (j === g ? { tabs, active: Math.max(0, Math.min(x.active >= i ? x.active - 1 : x.active, tabs.length - 1)) } : x))
    let focus = s.focus
    if (!tabs.length && groups.length > 1) {
      groups = groups.filter((_, j) => j !== g)
      focus = Math.min(focus > g ? focus - 1 : focus, groups.length - 1)
    }
    const dirty = { ...s.dirty }
    delete dirty[id]
    return { groups, focus, dirty }
  })
}

// Move a tab to group to (to === groups.length means a new group on the right).
export function moveTab(from: number, i: number, to: number) {
  setState((s) => {
    const t = s.groups[from]?.tabs[i]
    if (!t || (to === from && s.groups[from].tabs.length === 1)) return {}
    if (to >= s.groups.length && s.groups.length >= MAX_GROUPS) return {}
    let groups = s.groups.map((g) => ({ ...g, tabs: [...g.tabs] }))
    groups[from].tabs.splice(i, 1)
    groups[from].active = Math.max(0, Math.min(groups[from].active, groups[from].tabs.length - 1))
    if (to >= groups.length) groups.push({ tabs: [], active: 0 })
    groups[to].tabs.push(t)
    groups[to].active = groups[to].tabs.length - 1
    let focus = to
    if (!groups[from].tabs.length) {
      groups = groups.filter((_, j) => j !== from)
      if (to > from) focus--
    }
    return { groups, focus }
  })
}

// Merge all tabs into one group.
export function mergeGroups() {
  setState((s) => {
    const cur = activeTab(s)
    const tabs = s.groups.flatMap((g) => g.tabs)
    return { groups: [{ tabs, active: Math.max(0, cur ? tabs.indexOf(cur) : 0) }], focus: 0 }
  })
}

// Close the tabs of a session (after "Close" on the session).
export function closeSessionTabs(agent: string, session: string) {
  for (;;) {
    const [g, i] = findTab(state.groups, `s:${agent}/${session}`)
    if (g < 0) return
    closeTab(g, i)
  }
}

export const isMobile = () => window.matchMedia('(max-width: 760px)').matches

const mobileQuery = window.matchMedia('(max-width: 760px)')

export function useMobile(): boolean {
  const [m, setM] = useState(mobileQuery.matches)
  useEffect(() => {
    const f = () => setM(mobileQuery.matches)
    mobileQuery.addEventListener('change', f)
    return () => mobileQuery.removeEventListener('change', f)
  }, [])
  return m
}
