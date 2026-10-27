import { useEffect, useRef, useState } from 'preact/hooks'
import { ApiError, api, enc, filesPath, get, lsGet, lsSet } from '../api'
import type { EditorHandle } from '../editor/cm'
import { Icon } from '../icons'
import { getState, pinTab, setState, tabId, toast, useStore, type FileMode, type FileTab } from '../store'
import { socket } from '../ws'
import { hasView, Viewer, type Loaded } from './Viewer'

const modeKey = (kind: string) => `laavu.mode.${kind}`
const textual = (kind: string) => kind === 'markdown' || kind === 'code' || kind === 'text' || kind === 'html' || kind === 'svg'

// Unsaved tab text: survives switching modes and tabs.
const drafts = new Map<string, string>()

// Which modes a file of this kind has.
function modesFor(kind: string, git: boolean, editable: boolean): [FileMode, string][] {
  const m: [FileMode, string][] = []
  if (hasView(kind)) m.push(['view', 'Preview'], ['source', 'Source'])
  else if (textual(kind)) m.push(['source', 'Text'])
  else m.push(['view', 'Preview'])
  if (editable) m.push(['edit', 'Edit'])
  if (git && textual(kind)) m.push(['diff', 'Changes'])
  return m
}

// useFile loads a file and reloads it when it changes on disk.
function useFile(tab: FileTab): [Loaded | null, string, () => void] {
  const [data, setData] = useState<Loaded | null>(null)
  const [err, setErr] = useState('')
  const [rev, setRev] = useState(0)
  useEffect(() => {
    let cancelled = false
    get<Loaded>(`${filesPath(tab.agent, tab.folder)}/text?path=${enc(tab.path)}`)
      .then((d) => {
        if (cancelled) return
        setData(d)
        setErr('')
      })
      .catch((e) => !cancelled && setErr(e.message))
    return () => {
      cancelled = true
    }
  }, [tab.agent, tab.folder, tab.path, rev])
  useEffect(() => {
    socket.watch(tab.agent, tab.folder)
    const off = socket.on((m) => {
      if (m.t === 'fs' && m.agent === tab.agent && m.folder === tab.folder && (m.files as string[]).includes(tab.path)) setRev((r) => r + 1)
    })
    return () => {
      off()
      socket.unwatch(tab.agent, tab.folder)
    }
  }, [tab.agent, tab.folder, tab.path])
  return [data, err, () => setRev((r) => r + 1)]
}

interface Base {
  version: string
  content: string
}

export function FileView({ tab, visible }: { tab: FileTab; visible: boolean }) {
  const id = tabId(tab)
  const [data, err, reload] = useFile(tab)
  const git = useStore((s) => !!s.workdirs[tab.agent]?.find((w) => w.name === tab.folder)?.git)
  const stored = useStore((s) => s.fileModes[id])
  const dirty = useStore((s) => !!s.dirty[id])
  // base is the version the edit started from. While there are no edits, it follows the disk.
  const [base, setBase] = useState<Base | null>(null)
  const [comparing, setComparing] = useState(false)
  const [saving, setSaving] = useState(false)
  const editor = useRef<EditorHandle | null>(null)

  useEffect(() => {
    if (!data || data.version === undefined) return
    if (!dirty || !base) setBase({ version: data.version, content: data.content ?? '' })
  }, [data])

  const kind = data?.meta.kind || ''
  const editable = textual(kind) && data?.version !== undefined
  const modes = modesFor(kind, git, editable)
  const fallback: FileMode = hasView(kind) ? ((lsGet(modeKey(kind)) as FileMode) === 'source' ? 'source' : 'view') : modes[0]?.[0] || 'view'
  const mode: FileMode = stored && modes.some(([m]) => m === stored) ? stored : fallback
  const conflict = dirty && !!base && !!data && data.version !== base.version

  function setMode(m: FileMode) {
    setState((s) => ({ fileModes: { ...s.fileModes, [id]: m } }))
    if (hasView(kind) && (m === 'view' || m === 'source')) lsSet(modeKey(kind), m)
  }

  function markDirty(d: boolean) {
    setState((s) => ({ dirty: { ...s.dirty, [id]: d } }))
  }

  function onChange(doc: string) {
    drafts.set(id, doc)
    const d = !!base && doc !== base.content
    if (d !== dirty) markDirty(d)
    if (d && tab.preview) pinTab(id)
  }

  async function save(over?: Base) {
    const from = over || base
    if (!from || saving) return
    const content = drafts.get(id) ?? from.content
    setSaving(true)
    try {
      const r = await api<{ version: string }>('PUT', `${filesPath(tab.agent, tab.folder)}/file?path=${enc(tab.path)}`, { content, base: from.version })
      setBase({ version: r.version, content })
      drafts.delete(id)
      markDirty(false)
      setComparing(false)
      editor.current?.setCompare(null)
    } catch (e: any) {
      if (e instanceof ApiError && e.status === 409) {
        toast('File changed on disk: compare and save again')
        reload()
      } else toast(e.message)
    } finally {
      setSaving(false)
    }
  }

  function takeDisk() {
    if (!data) return
    drafts.delete(id)
    setBase({ version: data.version!, content: data.content ?? '' })
    editor.current?.setCompare(null)
    editor.current?.setDoc(data.content ?? '')
    setComparing(false)
    markDirty(false)
  }

  function toggleCompare() {
    if (!data) return
    const on = !comparing
    if (mode !== 'edit') setMode('edit')
    setComparing(on)
    setTimeout(() => editor.current?.setCompare(on ? data.content ?? '' : null), 0)
  }

  // Previewing unsaved text: show the draft, not the disk.
  const shown: Loaded | null = data && dirty && drafts.has(id) ? { ...data, content: drafts.get(id) } : data
  const parts = tab.path.split('/')
  const download = `${filesPath(tab.agent, tab.folder)}/download?path=${enc(tab.path)}`
  return (
    <>
      <div class="file-head">
        <span class="crumbs" title={`${tab.folder}/${tab.path}`}>
          {tab.folder} › {parts.slice(0, -1).map((p) => `${p} › `)}
          <b>{parts[parts.length - 1]}</b>
        </span>
        {modes.length > 1 && (
          <div class="seg" role="group" aria-label="Mode">
            {modes.map(([m, label]) => (
              <button class={mode === m ? 'on' : ''} onClick={() => setMode(m)}>
                {label}
              </button>
            ))}
          </div>
        )}
        {(dirty || mode === 'edit') && editable && (
          <button class="primary" style="height: 28px" disabled={!dirty || saving} onClick={() => save()} title="Ctrl+S">
            Save
          </button>
        )}
        <a class="ib btn" href={download} title="Download" aria-label="Download">
          <Icon name="download" size={16} />
        </a>
      </div>
      {conflict && (
        <div class="conflict" role="alert">
          <Icon name="alert" size={15} stroke={2} />
          <span>{comparing ? 'Comparing with the disk version: green is yours, red is from disk.' : 'The file changed on disk while you were editing.'}</span>
          <span class="spacer" />
          <button onClick={toggleCompare}>{comparing ? 'Hide comparison' : 'Compare'}</button>
          <button onClick={takeDisk}>Take from disk</button>
          <button class="primary" onClick={() => data && save({ version: data.version!, content: data.content ?? '' })}>
            Save mine
          </button>
        </div>
      )}
      <div class="file-body">
        {err && <p class="error pad">{err}</p>}
        {!data && !err && <p class="muted pad">Loading…</p>}
        {data && visible && mode === 'diff' && <DiffView tab={tab} rev={data.meta.mtime} />}
        {data && mode === 'edit' && base && (
          <EditorPane
            id={id}
            path={tab.path}
            base={base}
            editor={editor}
            onChange={onChange}
            onSave={() => (conflict ? toast('File changed on disk: compare first') : save())}
            focus={visible}
          />
        )}
        {shown && (mode === 'view' || mode === 'source') && <Viewer agent={tab.agent} folder={tab.folder} path={tab.path} data={shown} mode={mode} />}
      </div>
    </>
  )
}

function EditorPane({ id, path, base, editor, onChange, onSave, focus }: { id: string; path: string; base: Base; editor: { current: EditorHandle | null }; onChange: (doc: string) => void; onSave: () => void; focus: boolean }) {
  const el = useRef<HTMLDivElement>(null)
  // Handlers change on every render; the editor calls the current ones through a ref.
  const cb = useRef({ onChange, onSave })
  cb.current = { onChange, onSave }

  useEffect(() => {
    let h: EditorHandle | null = null
    let cancelled = false
    import('../editor/cm').then((m) => {
      if (cancelled || !el.current) return
      h = m.createEditor(el.current, {
        doc: drafts.get(id) ?? base.content,
        path,
        onChange: (d) => cb.current.onChange(d),
        onSave: () => cb.current.onSave(),
      })
      editor.current = h
      if (focus) h.focus()
    })
    return () => {
      cancelled = true
      h?.destroy()
      if (editor.current === h) editor.current = null
    }
  }, [id])

  // The disk changed and there are no edits: pull in the new text.
  useEffect(() => {
    if (!drafts.has(id)) editor.current?.setDoc(base.content)
  }, [base.version])

  return <div class="editor" ref={el} />
}

// DiffView: the working tree file against the last commit.
function DiffView({ tab, rev }: { tab: FileTab; rev: number }) {
  const [text, setText] = useState<string | null>(null)
  useEffect(() => {
    get<{ diff: string }>(`${filesPath(tab.agent, tab.folder)}/diff?path=${enc(tab.path)}&since=${Math.floor(Date.now() / 1000)}`)
      .then((r) => setText(r.diff))
      .catch((e) => setText(`error: ${e.message}`))
  }, [tab.agent, tab.folder, tab.path, rev])
  if (text === null) return <p class="muted pad">Loading…</p>
  if (!text.trim()) return <p class="muted pad">No changes since the last commit.</p>
  return (
    <pre class="diff mono">
      {text.split('\n').map((l) => (
        <div class={l.startsWith('+++') || l.startsWith('---') || l.startsWith('diff ') || l.startsWith('index ') ? 'meta' : l.startsWith('+') ? 'add' : l.startsWith('-') ? 'del' : l.startsWith('@@') ? 'hunk' : ''}>{l || ' '}</div>
      ))}
    </pre>
  )
}

// Closing a file tab with unsaved edits needs confirmation.
export function confirmClose(id: string): boolean {
  if (!getState().dirty[id]) {
    drafts.delete(id)
    return true
  }
  if (!confirm('The file has unsaved edits. Close without saving?')) return false
  drafts.delete(id)
  return true
}
