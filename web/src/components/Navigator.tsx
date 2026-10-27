import { useEffect, useRef, useState } from 'preact/hooks'
import { enc, filesPath, get, lsGet, lsSet, upload, type Changes, type Entry, type FileChange } from '../api'
import { Icon } from '../icons'
import { activeTab, getState, openTab, setState, toast, useMobile, useStore } from '../store'
import { socket } from '../ws'
import { insertText } from './KeyBar'

type Tree = Record<string, Entry[] | undefined>

const join = (dir: string, name: string) => (dir ? `${dir}/${name}` : name)
// File type by extension: the tree shows it with an icon and a quiet tint.
const types: [string, string, RegExp][] = [
  ['code', 'fileCode', /\.(c|cc|cpp|cxx|h|hpp|go|rs|py|rb|php|java|kt|kts|scala|swift|m|cs|fs|js|mjs|cjs|jsx|ts|tsx|vue|svelte|sh|bash|zsh|fish|ps1|lua|pl|r|jl|ex|exs|erl|hs|ml|clj|dart|zig|nim|sql|css|scss|sass|less|html?|mk)$|^(makefile|dockerfile|justfile|rakefile|gemfile)$/i],
  ['data', 'fileData', /\.(json|jsonc|json5|ya?ml|toml|ini|cfg|conf|env|xml|csv|tsv|lock|properties|plist|proto|graphql|gql|tf|hcl|nix|ipynb)$|^\.(env.*|gitignore|gitattributes|editorconfig|npmrc|dockerignore)$/i],
  ['doc', 'fileText', /\.(md|markdown|mdx|txt|rst|adoc|org|tex|pdf|docx?|odt|rtf|log)$|^(readme|license|copying|changelog|authors|notice)(\..*)?$/i],
  ['image', 'fileImage', /\.(png|jpe?g|gif|webp|avif|bmp|ico|svg|tiff?|heic)$/i],
  ['archive', 'fileArchive', /\.(zip|tar|gz|tgz|bz2|xz|zst|7z|rar|deb|rpm|jar|whl)$/i],
]
function fileType(name: string): [string, string] {
  for (const [t, icon, re] of types) if (re.test(name)) return [t, icon]
  return ['other', 'file']
}

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

  // Changes always load: the tab counter and status letters in the tree need them.
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
    <aside class="nav" aria-label="Files">
      <div class="nav-head">
        <div class="seg" role="tablist">
          <button class={tabSel === 'files' ? 'on' : ''} onClick={() => setState({ navTab: 'files', mobileScreen: mobile ? 'files' : screen })}>
            Files
          </button>
          <button class={tabSel === 'changes' ? 'on' : ''} onClick={() => setState({ navTab: 'changes', mobileScreen: mobile ? 'changes' : screen })}>
            Changes{changed.length > 0 && <span class="count">{changed.length}</span>}
          </button>
        </div>
        <span class="spacer" />
        {tabSel === 'files' && (
          <button class={'ib' + (hidden ? ' on' : '')} title={hidden ? 'Hide hidden files' : 'Show hidden files'} aria-label="hidden files" onClick={() => setState({ showHidden: !hidden })}>
            <Icon name={hidden ? 'eye' : 'eyeOff'} size={15} />
          </button>
        )}
      </div>
      {!folder ? (
        <p class="muted pad">Pick a workspace or a session.</p>
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

// In a repository, the working tree against the last commit; otherwise, files changed since the mark.
function changedFiles(ch: Changes | null): FileChange[] {
  if (!ch) return []
  return (ch.git ? ch.uncommitted : ch.files) || []
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
      // Refresh only loaded directories touched by the changes.
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
        toast(`Uploaded: ${r.path}`)
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
            {isDir ? <Icon name={open[p] ? 'folderOpen' : 'folder'} size={15} class="ficon dir" /> : <FileIcon name={e.name} />}
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

function FileIcon({ name }: { name: string }) {
  const [t, icon] = fileType(name)
  return <Icon name={icon} size={15} class={'ficon t-' + t} />
}

function ChangesList({ agent, folder, changes, since, onSeen }: { agent: string; folder: string; changes: Changes | null; since: number; onSeen: () => void }) {
  const git = !!changes?.git
  const files = changedFiles(changes)
  const open = (f: FileChange) => openTab({ kind: 'file', agent, folder, path: f.path }, { mode: git ? 'diff' : undefined })
  const row = (f: FileChange) => (
    <div class="change" onClick={() => open(f)} data-path={f.path} title={f.path}>
      <span class={'chg chg-' + (f.status || '').replace(/\?/g, 'q')}>{(f.status || '').replace('??', 'U')}</span>
      <span class="path">{f.path}</span>
    </div>
  )
  return (
    <>
      <div class="nav-body changes" data-testid="changes">
        {changes && (
          <div class="muted small" style="padding: 4px 6px 8px">
            {git ? 'Working tree against the last commit' : `Since ${new Date(since * 1000).toLocaleString('en')}`}
          </div>
        )}
        {!changes && <p class="muted small pad">Loading…</p>}
        {changes && (files.length ? <div class="changes-list">{files.map(row)}</div> : <p class="muted small">{git ? 'Working tree clean.' : 'Nothing changed.'}</p>)}
        {changes?.truncated && <p class="muted small">List truncated.</p>}
      </div>
      {changes && !git && (
        <div class="nav-foot">
          <button
            style="flex: 1"
            onClick={() => {
              lsSet(lastSeenKey(agent, folder), String(Math.floor(Date.now() / 1000)))
              onSeen()
              toast('Marked as seen')
            }}
          >
            Mark as seen
          </button>
        </div>
      )}
    </>
  )
}
