import { useEffect, useRef, useState } from 'preact/hooks'
import { agentPath, get, lsGet, lsSet, post, type Action, type SessionInfo, type Workdir } from '../api'
import { Icon } from '../icons'
import { openTab, setState, toast, useStore } from '../store'
import { Dot, statusLabel } from './Status'

export async function loadWorkdirs(agent: string) {
  try {
    const list = await get<Workdir[]>(`${agentPath(agent)}/workdirs`)
    setState((s) => ({ workdirs: { ...s.workdirs, [agent]: list } }))
  } catch (e: any) {
    toast(e.message)
  }
}

// refreshWorkdirs — перечитать папки не чаще раза в пару секунд: git status
// по всем репозиториям агента не бесплатный.
const refreshTimers: Record<string, number> = {}
export function refreshWorkdirs(agent: string) {
  if (refreshTimers[agent]) return
  refreshTimers[agent] = window.setTimeout(() => {
    delete refreshTimers[agent]
    loadWorkdirs(agent)
  }, 2000)
}

export async function newSession(agent: string, folder: string, kind: 'claude' | 'shell', create = false) {
  try {
    const si = await post<SessionInfo>(`${agentPath(agent)}/sessions`, { folder, kind, create, cols: 120, rows: 36 })
    setState((s) => ({ sessions: { ...s.sessions, [agent]: mergeSession(s.sessions[agent], si) } }))
    openTab({ kind: 'session', agent, session: si.name })
    if (create) loadWorkdirs(agent)
  } catch (e: any) {
    toast(e.message)
  }
}

function mergeSession(list: SessionInfo[] | undefined, si: SessionInfo): SessionInfo[] {
  const l = (list || []).filter((s) => s.name !== si.name)
  return [...l, si].sort((a, b) => a.name.localeCompare(b.name))
}

export async function createFolder(agent: string, name: string): Promise<boolean> {
  try {
    await post(`${agentPath(agent)}/workdirs`, { name })
    await loadWorkdirs(agent)
    setState({ nav: { agent, folder: name } })
    return true
  } catch (err: any) {
    toast(err.message)
    return false
  }
}

const collapsedKey = (agent: string) => `laavu.collapsed.${agent}`

// actionLabel — «Edit src/app.ts»: что агент делал последним.
export function actionLabel(a?: Action) {
  return a ? (a.target ? `${a.tool} ${a.target}` : a.tool) : ''
}

function kindLabel(s: SessionInfo) {
  return s.kind === 'claude' ? 'агент' : s.kind === 'shell' ? 'терминал' : s.command || ''
}

export function Sidebar() {
  const agent = useStore((s) => s.agent)
  const workdirs = useStore((s) => s.workdirs[s.agent])
  const sessions = useStore((s) => s.sessions[s.agent])
  const context = useStore((s) => s.context)
  const groups = useStore((s) => s.groups)
  const focus = useStore((s) => s.focus)
  const nav = useStore((s) => s.nav)
  const [q, setQ] = useState('')
  const [newFolder, setNewFolder] = useState<string | null>(null)
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({})
  const search = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (!agent) return
    loadWorkdirs(agent)
    try {
      setCollapsed(JSON.parse(lsGet(collapsedKey(agent)) || '{}'))
    } catch {
      setCollapsed({})
    }
  }, [agent])

  // Ctrl+K — поиск по сессиям и папкам.
  useEffect(() => {
    const f = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setState({ sideOpen: true, mobileScreen: 'sessions' })
        setTimeout(() => search.current?.focus(), 0)
      }
    }
    window.addEventListener('keydown', f)
    return () => window.removeEventListener('keydown', f)
  }, [])

  function toggle(folder: string) {
    const next = { ...collapsed, [folder]: !collapsed[folder] }
    setCollapsed(next)
    lsSet(collapsedKey(agent), JSON.stringify(next))
  }

  const cur = groups[focus]?.tabs[groups[focus].active]
  const needle = q.trim().toLowerCase()
  const match = (s: string) => !needle || s.toLowerCase().includes(needle)
  const all = sessions || []
  const byFolder = new Map<string, SessionInfo[]>()
  for (const s of all) {
    const f = s.folder || ''
    if (!byFolder.has(f)) byFolder.set(f, [])
    byFolder.get(f)!.push(s)
  }
  const waiting = all.filter((s) => s.status === 'waiting' && match(s.name))
  const folders = (workdirs || []).filter((w) => match(w.name) || (byFolder.get(w.name) || []).some((s) => match(s.name)))
  const open = (s: SessionInfo) => openTab({ kind: 'session', agent, session: s.name })

  const sessionRow = (s: SessionInfo) => {
    const ctx = context[`${agent}/${s.name}`]
    const isCur = cur?.kind === 'session' && cur.agent === agent && cur.session === s.name
    return (
      <li class={'sess' + (isCur ? ' current' : '') + (s.sleeping ? ' sleeping' : '')} onClick={() => open(s)} data-session={s.name}>
        <span class="row">
          {s.kind === 'claude' ? <Dot status={s.status} /> : <Icon name="terminal" size={13} class="muted" />}
          <span class="name">{s.name}</span>
          {ctx && s.kind === 'claude' ? <span class={'pct' + (ctx.percent >= 65 ? ' warn' : '')}>{ctx.percent}%</span> : <span class="pct">{s.kind !== 'claude' ? kindLabel(s) : ''}</span>}
        </span>
        {s.kind === 'claude' && (
          <span class="sub" title={actionLabel(s.action) || undefined}>
            {s.sleeping ? 'спит, поднимется при открытии' : statusLabel(s.status) || 'агент'}
            {!s.sleeping && s.action && <span class="act"> · {actionLabel(s.action)}</span>}
          </span>
        )}
      </li>
    )
  }

  async function submitFolder(e: Event) {
    e.preventDefault()
    const name = (newFolder || '').trim()
    if (name && (await createFolder(agent, name))) setNewFolder(null)
  }

  return (
    <aside class="side" aria-label="Сессии">
      <div class="side-head">
        <div class="side-title">
          <b>{agent || 'Laavu'}</b>
          <span class="muted small">
            {(workdirs || []).length} папок · {all.length} сессий
          </span>
        </div>
        <label class="search">
          <Icon name="search" size={15} stroke={2} />
          <input ref={search} placeholder="Сессия или папка…" aria-label="Найти сессию или папку" value={q} onInput={(e) => setQ((e.target as HTMLInputElement).value)} onKeyDown={(e) => e.key === 'Escape' && setQ('')} />
          <kbd class="desktop-only">Ctrl K</kbd>
        </label>
      </div>
      <div class="side-body">
        {!agent && <p class="muted pad">Нет доступных агентов.</p>}
        {waiting.length > 0 && (
          <section class="waiting">
            <div class="section-label">Ждут тебя · {waiting.length}</div>
            {waiting.map((s) => (
              <button class="wait-card" onClick={() => open(s)} data-waiting={s.name}>
                <span class="row">
                  <Dot status="waiting" />
                  {s.name}
                </span>
                <span class="sub">{s.folder || 'прочее'} · ждёт ответа</span>
              </button>
            ))}
          </section>
        )}
        <ul class="folders">
          {folders.map((w) => {
            const list = (byFolder.get(w.name) || []).filter((s) => match(s.name) || match(w.name))
            const isOpen = needle ? true : !collapsed[w.name]
            return (
              <li class={'folder' + (nav.agent === agent && nav.folder === w.name ? ' current' : '')} data-folder={w.name}>
                <div
                  class="folder-head"
                  onClick={() => {
                    toggle(w.name)
                    setState({ nav: { agent, folder: w.name } })
                  }}
                >
                  <Icon name={isOpen ? 'chevronDown' : 'chevronRight'} size={13} stroke={2} />
                  <span class="fname">{w.name}</span>
                  {w.git && (
                    <span class="meta branch" title={`ветка ${w.branch || '—'}`}>
                      <Icon name="git" size={11} />
                      {w.branch || '—'}
                    </span>
                  )}
                  {!!w.changes && (
                    <button
                      class="chg-count"
                      title={`Незакоммиченных изменений: ${w.changes}`}
                      aria-label={`изменений: ${w.changes}`}
                      onClick={(e) => {
                        e.stopPropagation()
                        setState({ nav: { agent, folder: w.name }, navTab: 'changes', navOpen: true, mobileScreen: 'changes' })
                      }}
                    >
                      ±{w.changes}
                    </button>
                  )}
                  <span class="spacer" />
                  <span class="acts" onClick={(e) => e.stopPropagation()}>
                    <button class="ib sm" title={`Новая сессия агента в ${w.name}`} aria-label="+ сессия" onClick={() => newSession(agent, w.name, 'claude')}>
                      <Icon name="plus" size={14} stroke={2} />
                    </button>
                    <button class="ib sm" title={`Новый терминал в ${w.name}`} aria-label="+ терминал" onClick={() => newSession(agent, w.name, 'shell')}>
                      <Icon name="terminal" size={14} stroke={2} />
                    </button>
                    <button class="ib sm" title="Что изменилось" aria-label="что изменилось" onClick={() => setState({ nav: { agent, folder: w.name }, navTab: 'changes', navOpen: true, mobileScreen: 'changes' })}>
                      <Icon name="git" size={14} />
                    </button>
                  </span>
                  {!isOpen && list.length > 0 && <span class="meta">{list.length}</span>}
                </div>
                {isOpen && (list.length ? <ul class="sessions">{list.map(sessionRow)}</ul> : !needle && <div class="empty-folder">Нет сессий</div>)}
              </li>
            )
          })}
          {byFolder.has('') && (
            <li class="folder" data-folder="">
              <div class="folder-head">
                <span class="muted">прочее</span>
              </div>
              <ul class="sessions">{byFolder.get('')!.filter((s) => match(s.name)).map(sessionRow)}</ul>
            </li>
          )}
        </ul>
        {newFolder !== null && (
          <form class="new-folder" onSubmit={submitFolder}>
            <input
              autoFocus
              placeholder="имя папки"
              aria-label="Имя новой папки"
              value={newFolder}
              onInput={(e) => setNewFolder((e.target as HTMLInputElement).value)}
              onKeyDown={(e) => e.key === 'Escape' && setNewFolder(null)}
              pattern="[A-Za-z0-9_][A-Za-z0-9._\-]*"
              title="латиница, цифры, точка, дефис, подчёркивание"
            />
            <button type="submit">Создать</button>
          </form>
        )}
      </div>
      <div class="side-foot">
        <button class="primary" onClick={() => setState({ modal: { kind: 'newSession' } })} disabled={!agent}>
          <Icon name="plus" size={15} stroke={2.4} />
          Новая сессия
        </button>
        <button class="ib" style="width: 36px; height: 36px; border: 1px solid var(--line)" title="Новая папка" aria-label="Новая папка" onClick={() => setNewFolder(newFolder === null ? '' : null)} disabled={!agent}>
          <Icon name="folderPlus" size={17} />
        </button>
      </div>
    </aside>
  )
}
