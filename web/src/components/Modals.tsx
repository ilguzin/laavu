import { useEffect, useRef, useState } from 'preact/hooks'
import { Terminal } from '@xterm/xterm'
import { agentPath, get, post, sessPath, type SSHKey, type Workdir } from '../api'
import { Icon } from '../icons'
import { getState, setState, toast, useStore, type SessTab } from '../store'
import { termTheme } from '../theme'
import { UserMenuItems } from './Rail'
import { createFolder, loadWorkdirs, newSession } from './Sidebar'

export function Modals() {
  const modal = useStore((s) => s.modal)
  if (!modal) return null
  const close = () => setState({ modal: null })
  return (
    <div class="modal-bg" onClick={(e) => e.target === e.currentTarget && close()}>
      <div class={'modal modal-' + modal.kind} role="dialog">
        <button class="ib modal-close" onClick={close} aria-label="close">
          <Icon name="x" size={16} />
        </button>
        {modal.kind === 'history' && <History tab={modal.data} />}
        {modal.kind === 'device' && <DeviceLink url={modal.data} />}
        {modal.kind === 'newSession' && <NewSession />}
        {modal.kind === 'agentSettings' && <AgentSettings agent={modal.data} />}
        {modal.kind === 'menu' && (
          <div class="popover" style="position: static; box-shadow: none; border: 0; padding: 24px 0 0" onClick={close}>
            <UserMenuItems />
          </div>
        )}
      </div>
    </div>
  )
}

// Pane history from tmux capture-pane: a separate xterm with scrolling, selection and the browser's find.
function History({ tab }: { tab: SessTab }) {
  const el = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const term = new Terminal({
      disableStdin: true,
      scrollback: 50000,
      convertEol: true,
      fontFamily: '"JetBrains Mono", ui-monospace, monospace',
      fontSize: 13,
      theme: termTheme(),
    })
    term.open(el.current!)
    get<{ text: string }>(`${sessPath(tab.agent, tab.session)}/history`)
      .then((h) => {
        term.write(h.text.replace(/\n$/, ''))
        term.scrollToBottom()
      })
      .catch((e) => toast(e.message))
    return () => term.dispose()
  }, [tab.agent, tab.session])
  return (
    <>
      <h3>History: {tab.session}</h3>
      <div class="history-term" ref={el} />
    </>
  )
}

// folderFromURL turns git@github.com:org/my-repo.git into my-repo, fit for a folder name.
export function folderFromURL(url: string) {
  const last = url.trim().replace(/\/+$/, '').split(/[/:]/).pop() || ''
  return last.replace(/\.git$/, '').replace(/[^A-Za-z0-9._-]/g, '-').replace(/^[.-]+/, '')
}

// A git error that a key on the agent would fix.
const needsKey = (err: string) => /Permission denied \(publickey|Host key verification failed|Could not read from remote repository/.test(err)

const namePattern = '[A-Za-z0-9_][A-Za-z0-9._\\-]*'
const nameTitle = 'Latin letters, digits, dot, hyphen, underscore'

// New session: in a workspace (existing or new) or in a fresh git clone, agent or terminal.
function NewSession() {
  const s = getState()
  const agent = s.agent
  const workdirs = useStore((st) => st.workdirs[agent] || [])
  const def = s.nav.agent === agent && s.nav.folder ? s.nav.folder : workdirs[0]?.name || '\0new'
  const [source, setSource] = useState<'folder' | 'clone'>('folder')
  const [folder, setFolder] = useState(def)
  const [name, setName] = useState('')
  const [url, setURL] = useState('')
  const [cloneName, setCloneName] = useState<string | null>(null) // null: follows the address
  const [kind, setKind] = useState<'claude' | 'shell'>('claude')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const isNew = folder === '\0new'
  const target = cloneName ?? folderFromURL(url)

  async function submit(e: Event) {
    e.preventDefault()
    setBusy(true)
    setErr('')
    let f = folder
    if (source === 'clone') {
      try {
        await post<Workdir>(`${agentPath(agent)}/clone`, { url: url.trim(), folder: target })
      } catch (e: any) {
        setErr(e.message)
        return setBusy(false)
      }
      f = target
      await loadWorkdirs(agent)
      setState({ nav: { agent, folder: f } })
    } else if (isNew) {
      f = name.trim()
      if (!f || !(await createFolder(agent, f))) return setBusy(false)
    }
    await newSession(agent, f, kind)
    setState({ modal: null })
  }

  return (
    <form class="form-col" onSubmit={submit}>
      <h3>New session · {agent}</h3>
      <div class="kind-pick">
        <button type="button" class={kind === 'claude' ? 'on' : ''} onClick={() => setKind('claude')}>
          <Icon name="agent" size={20} />
          Agent
        </button>
        <button type="button" class={kind === 'shell' ? 'on' : ''} onClick={() => setKind('shell')}>
          <Icon name="terminal" size={20} />
          Terminal
        </button>
      </div>
      <div class="seg" role="tablist" aria-label="Where">
        <button type="button" class={source === 'folder' ? 'on' : ''} onClick={() => setSource('folder')}>
          Workspace
        </button>
        <button type="button" class={source === 'clone' ? 'on' : ''} onClick={() => setSource('clone')}>
          Clone from git
        </button>
      </div>
      {source === 'folder' && (
        <>
          <label>
            Workspace
            <select value={folder} onChange={(e) => setFolder((e.target as HTMLSelectElement).value)}>
              {workdirs.map((w) => (
                <option value={w.name}>{w.name}</option>
              ))}
              <option value={'\0new'}>New workspace…</option>
            </select>
          </label>
          {(isNew || !workdirs.length) && (
            <label>
              New workspace name
              <input autoFocus required value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} pattern={namePattern} title={nameTitle} />
            </label>
          )}
        </>
      )}
      {source === 'clone' && (
        <>
          <label>
            Repository
            <input
              autoFocus
              required
              placeholder="git@github.com:org/repo.git"
              aria-label="Repository address"
              autocapitalize="off"
              spellcheck={false}
              value={url}
              onInput={(e) => setURL((e.target as HTMLInputElement).value)}
            />
          </label>
          <label>
            Into new workspace
            <input required aria-label="Clone workspace" value={target} onInput={(e) => setCloneName((e.target as HTMLInputElement).value)} pattern={namePattern} title={nameTitle} />
          </label>
          <span class="muted small">Over ssh, git uses the agent's key, see agent settings.</span>
        </>
      )}
      {err && (
        <div class="error" data-testid="clone-error">
          <pre class="git-error">{err}</pre>
          {needsKey(err) && (
            <a onClick={() => setState({ modal: { kind: 'agentSettings', data: agent } })}>Agent settings: ssh key</a>
          )}
        </div>
      )}
      <div class="modal-actions">
        <button type="button" onClick={() => setState({ modal: null })}>
          Cancel
        </button>
        <button class="primary" type="submit" disabled={busy}>
          {busy && source === 'clone' ? 'Cloning…' : 'Start'}
        </button>
      </div>
    </form>
  )
}

// Agent settings. For now, the ssh key git and ssh use in the agent's sessions.
function AgentSettings({ agent }: { agent: string }) {
  const [key, setKey] = useState<SSHKey | null>(null)
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    get<SSHKey>(`${agentPath(agent)}/sshkey`).then(setKey, (e) => toast(e.message))
  }, [agent])

  async function create() {
    setBusy(true)
    try {
      setKey(await post<SSHKey>(`${agentPath(agent)}/sshkey`))
    } catch (e: any) {
      toast(e.message)
    }
    setBusy(false)
  }

  return (
    <div class="form-col">
      <h3>Agent settings · {agent}</h3>
      <b>
        <Icon name="key" size={15} /> SSH key
      </b>
      {key === null && <span class="muted small">Loading…</span>}
      {key && !key.public && (
        <>
          <span class="muted small">The agent has no ssh key. Create one to clone and push over ssh. The private key stays on the server.</span>
          <div>
            <button class="primary" onClick={create} disabled={busy}>
              Create key
            </button>
          </div>
        </>
      )}
      {key?.public && (
        <>
          <textarea class="key" readOnly rows={4} aria-label="Public key" value={key.public} onFocus={(e) => (e.target as HTMLTextAreaElement).select()} />
          <span class="muted small">
            {key.fingerprint}. Add the key to the repository as a deploy key (write access if the agent should push) or to your git account. In the agent's sessions it is
            ~/.ssh/id_ed25519.
          </span>
          <div>
            <button onClick={() => navigator.clipboard?.writeText(key.public!).then(() => toast('Copied'))}>Copy</button>
          </div>
        </>
      )}
    </div>
  )
}

function DeviceLink({ url }: { url: string }) {
  return (
    <>
      <h3>Add device</h3>
      <p>Open the link on the new device within 15 minutes and register a passkey there.</p>
      <input class="link" readOnly value={url} onFocus={(e) => (e.target as HTMLInputElement).select()} />
      <div class="modal-actions">
        <button onClick={() => navigator.clipboard?.writeText(url).then(() => toast('Copied'))}>Copy</button>
      </div>
    </>
  )
}
