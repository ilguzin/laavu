import { useEffect, useRef, useState } from 'preact/hooks'
import { enc, filesPath, get, lsGet, lsSet, upload, type Changes, type Entry, type FileChange } from '../api'
import { Icon } from '../icons'
import { activeTab, getState, openTab, setState, toast, useMobile, useStore } from '../store'
import { socket } from '../ws'
import { insertText } from './KeyBar'

type Tree = Record<string, Entry[] | undefined>

const join = (dir: string, name: string) => (dir ? `${dir}/${name}` : name)
const lastSeenKey = (agent: string, folder: string) => `laavu.seen.${agent}/${folder}`

export function Navigator() {
  const nav = useStore((s) => s.nav)
  const navTab = useStore((s) => s.navTab)
  const screen = useStore((s) => s.mobileScreen)
  const hidden = useStore((s) => s.showHidden)
  const mobile = useMobile()
  const { agent, folder } = nav
  const tabSel = mobile ? (screen === 'changes' ? 'changes' : 'files') : navTab
  const [changes, setChanges] = useState<Changes | null>(null)
  const [seen, setSeen] = useState(0)
  const [rev, setRev] = useState(0)

  // Изменения грузятся всегда: счётчик на вкладке и буквы статуса в дереве.
  useEffect(() => {
    if (!agent || !folder) return
    setChanges(null)
    const stored = parseInt(lsGet(lastSeenKey(agent, folder)) || '0', 10)
    const since = stored || Math.floor(Date.now() / 1000) - 86400
    setSeen(since)
    let timer = 0
    const load = () =>
      get<Changes>(`${filesPath(agent, folder)}/changes?since=${since}`)
        .then(setChanges)
        .catch(() => setChanges({ git: false, files: [] }))
    load()
    socket.watch(agent, folder)
    const off = socket.on((m) => {
      if (m.t !== 'fs' || m.agent !== agent || m.folder !== folder) return
      clearTimeout(timer)
      timer = window.setTimeout(load, 1500)
    })
    return () => {
      clearTimeout(timer)
      off()
      socket.unwatch(agent, folder)
    }
  }, [agent, folder, rev])

  const changed = changedFiles(changes)
  const status: Record<string, string> = {}
  for (const f of changes?.uncommitted || []) status[f.path] = f.status || 'M'

  return (
    <aside class="nav" aria-label="Файлы">
      <div class="nav-head">
        <div class="seg" role="tablist">
          <button class={tabSel === 'files' ? 'on' : ''} onClick={() => setState({ navTab: 'files', mobileScreen: mobile ? 'files' : screen })}>
            Файлы
          </button>
          <button class={tabSel === 'changes' ? 'on' : ''} onClick={() => setState({ navTab: 'changes', mobileScreen: mobile ? 'changes' : screen })}>
            Изменения{changed.length > 0 && <span class="count">{changed.length}</span>}
          </button>
        </div>
        <span class="spacer" />
        {tabSel === 'files' && (
          <button class={'ib' + (hidden ? ' on' : '')} title={hidden ? 'Не показывать скрытые' : 'Показать скрытые'} aria-label="скрытые" onClick={() => setState({ showHidden: !hidden })}>
            <Icon name={hidden ? 'eye' : 'eyeOff'} size={15} />
          </button>
        )}
        <button class="ib desktop-only" title="Свернуть панель" aria-label="Свернуть панель файлов" onClick={() => setState({ navOpen: false })}>
          <Icon name="panelRight" size={16} />
        </button>
      </div>
      {!folder ? (
        <p class="muted pad">Выберите рабочую папку или сессию.</p>
      ) : (
        <>
          <div class="nav-folder">
            <Icon name="folder" size={13} />
            {agent} / {folder}
          </div>
          {tabSel === 'files' ? <FileTree agent={agent} folder={folder} hidden={hidden} status={status} /> : <ChangesList agent={agent} folder={folder} changes={changes} since={seen} onSeen={() => setRev((r) => r + 1)} />}
        </>
      )}
    </aside>
  )
}

function changedFiles(ch: Changes | null): FileChange[] {
  if (!ch) return []
  if (!ch.git) return ch.files || []
  const m = new Map<string, FileChange>()
  for (const c of ch.commits || []) for (const f of c.files) m.set(f.path, f)
  for (const f of ch.uncommitted || []) m.set(f.path, f)
  return [...m.values()]
}

function FileTree({ agent, folder, hidden, status }: { agent: string; folder: string; hidden: boolean; status: Record<string, string> }) {
  const groups = useStore((s) => s.groups)
  const focus = useStore((s) => s.focus)
  const [tree, setTree] = useState<Tree>({})
  const [open, setOpen] = useState<Record<string, boolean>>({ '': true })
  const loaded = useRef<Tree>({})
  loaded.current = tree

  async function load(dir: string) {
    try {
      const list = await get<Entry[]>(`${filesPath(agent, folder)}/tree?path=${enc(dir)}${hidden ? '&hidden=1' : ''}`)
      setTree((t) => ({ ...t, [dir]: list }))
    } catch {
      setTree((t) => ({ ...t, [dir]: [] }))
    }
  }

  useEffect(() => {
    setTree({})
    setOpen({ '': true })
    load('')
    socket.watch(agent, folder)
    const off = socket.on((m) => {
      if (m.t !== 'fs' || m.agent !== agent || m.folder !== folder) return
      // Обновляем только загруженные каталоги, которых коснулись изменения.
      for (const d of m.dirs as string[]) {
        const dir = d === '.' ? '' : d
        if (loaded.current[dir] !== undefined) load(dir)
      }
    })
    return () => {
      off()
      socket.unwatch(agent, folder)
    }
  }, [agent, folder, hidden])

  async function dropUpload(e: DragEvent, dir: string) {
    const files = e.dataTransfer?.files
    if (!files || !files.length) return
    e.preventDefault()
    e.stopPropagation()
    for (const f of Array.from(files)) {
      try {
        const r = await upload(agent, folder, f, dir, (p) => toast(`${f.name}: ${Math.round(p * 100)}%`))
        toast(`Загружено: ${r.path}`)
        const s = getState().lastSession
        if (s) insertText(s, r.path)
      } catch (err: any) {
        toast(err.message)
      }
    }
  }

  const cur = activeTab({ ...getState(), groups, focus })
  const curPath = cur?.kind === 'file' && cur.agent === agent && cur.folder === folder ? cur.path : ''

  const render = (dir: string, depth: number): any =>
    (tree[dir] || []).map((e) => {
      const p = join(dir, e.name)
      const isDir = e.type === 'dir'
      return (
        <>
          <li
            class={'entry' + (isDir ? ' dir' : '') + (p === curPath ? ' current' : '') + (e.hidden ? ' hidden-file' : '')}
            style={{ paddingLeft: `${6 + depth * 14}px` }}
            data-path={p}
            draggable={!isDir}
            onDragStart={(ev) => ev.dataTransfer?.setData('text/plain', `${folder}/${p}`)}
            onClick={() => {
              if (isDir) {
                setOpen((o) => ({ ...o, [p]: !o[p] }))
                if (!tree[p]) load(p)
              } else openTab({ kind: 'file', agent, folder, path: p })
            }}
            onDblClick={() => !isDir && openTab({ kind: 'file', agent, folder, path: p }, { pin: true })}
            onDragOver={(ev) => isDir && ev.dataTransfer?.types.includes('Files') && ev.preventDefault()}
            onDrop={(ev) => isDir && dropUpload(ev, p)}
          >
            <span class="caret">{isDir ? (open[p] ? '▾' : '▸') : ''}</span>
            <span class="name">{e.name}</span>
            {status[p] && <span class="st">{status[p].replace('??', 'U')}</span>}
          </li>
          {isDir && open[p] && render(p, depth + 1)}
        </>
      )
    })

  return (
    <div class="nav-body" onDragOver={(e) => e.dataTransfer?.types.includes('Files') && e.preventDefault()} onDrop={(e) => dropUpload(e, '')}>
      <ul class="tree">{render('', 0)}</ul>
    </div>
  )
}

function ChangesList({ agent, folder, changes, since, onSeen }: { agent: string; folder: string; changes: Changes | null; since: number; onSeen: () => void }) {
  const open = (f: FileChange) => openTab({ kind: 'file', agent, folder, path: f.path, since: changes?.git ? since : undefined }, { mode: changes?.git ? 'diff' : undefined })
  const row = (f: FileChange) => (
    <div class="change" onClick={() => open(f)} data-path={f.path} title={f.path}>
      <span class={'chg chg-' + (f.status || '').replace(/\?/g, 'q')}>{(f.status || '').replace('??', 'U')}</span>
      <span class="path">{f.path}</span>
    </div>
  )
  return (
    <>
      <div class="nav-body changes" data-testid="changes">
        <div class="muted small" style="padding: 4px 6px 8px">
          С {new Date(since * 1000).toLocaleString('ru')}
        </div>
        {!changes && <p class="muted small pad">Загрузка…</p>}
        {changes?.git && (
          <div class="changes-list">
            {!!changes.uncommitted?.length && (
              <div>
                <div class="commit-title">Не закоммичено</div>
                {changes.uncommitted.map(row)}
              </div>
            )}
            {(changes.commits || []).map((c) => (
              <div>
                <div class="commit-title">
                  <code>{c.hash.slice(0, 7)}</code> {c.subject}
                </div>
                {c.files.map(row)}
              </div>
            ))}
            {!changes.commits?.length && !changes.uncommitted?.length && <p class="muted small">Ничего не изменилось.</p>}
          </div>
        )}
        {changes && !changes.git && (changes.files?.length ? <div class="changes-list">{changes.files.map(row)}</div> : <p class="muted small">Ничего не изменилось.</p>)}
        {changes?.truncated && <p class="muted small">Список обрезан.</p>}
      </div>
      <div class="nav-foot">
        <button
          style="flex: 1"
          onClick={() => {
            lsSet(lastSeenKey(agent, folder), String(Math.floor(Date.now() / 1000)))
            onSeen()
            toast('Отмечено как просмотренное')
          }}
        >
          Отметить как просмотренное
        </button>
      </div>
    </>
  )
}
