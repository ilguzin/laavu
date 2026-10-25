import { useEffect, useRef, useState } from 'preact/hooks'
import { Terminal } from '@xterm/xterm'
import { get, sessPath } from '../api'
import { Icon } from '../icons'
import { getState, setState, toast, useStore, type SessTab } from '../store'
import { termTheme } from '../theme'
import { UserMenuItems } from './Rail'
import { createFolder, newSession } from './Sidebar'

export function Modals() {
  const modal = useStore((s) => s.modal)
  if (!modal) return null
  const close = () => setState({ modal: null })
  return (
    <div class="modal-bg" onClick={(e) => e.target === e.currentTarget && close()}>
      <div class={'modal modal-' + modal.kind} role="dialog">
        <button class="ib modal-close" onClick={close} aria-label="закрыть">
          <Icon name="x" size={16} />
        </button>
        {modal.kind === 'history' && <History tab={modal.data} />}
        {modal.kind === 'device' && <DeviceLink url={modal.data} />}
        {modal.kind === 'newSession' && <NewSession />}
        {modal.kind === 'menu' && (
          <div class="popover" style="position: static; box-shadow: none; border: 0; padding: 24px 0 0" onClick={close}>
            <UserMenuItems />
          </div>
        )}
      </div>
    </div>
  )
}

// История панели из tmux capture-pane: свой xterm с прокруткой, выделением и поиском браузера.
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
      <h3>История: {tab.session}</h3>
      <div class="history-term" ref={el} />
    </>
  )
}

// Новая сессия: папка (или новая папка) и вид.
function NewSession() {
  const s = getState()
  const agent = s.agent
  const workdirs = useStore((st) => st.workdirs[agent] || [])
  const def = s.nav.agent === agent && s.nav.folder ? s.nav.folder : workdirs[0]?.name || '\0new'
  const [folder, setFolder] = useState(def)
  const [name, setName] = useState('')
  const [kind, setKind] = useState<'claude' | 'shell'>('claude')
  const [busy, setBusy] = useState(false)
  const isNew = folder === '\0new'

  async function submit(e: Event) {
    e.preventDefault()
    setBusy(true)
    let f = folder
    if (isNew) {
      f = name.trim()
      if (!f || !(await createFolder(agent, f))) return setBusy(false)
    }
    await newSession(agent, f, kind)
    setState({ modal: null })
  }

  return (
    <form class="form-col" onSubmit={submit}>
      <h3>Новая сессия · {agent}</h3>
      <div class="kind-pick">
        <button type="button" class={kind === 'claude' ? 'on' : ''} onClick={() => setKind('claude')}>
          <Icon name="agent" size={20} />
          Агент
        </button>
        <button type="button" class={kind === 'shell' ? 'on' : ''} onClick={() => setKind('shell')}>
          <Icon name="terminal" size={20} />
          Терминал
        </button>
      </div>
      <label>
        Папка
        <select value={folder} onChange={(e) => setFolder((e.target as HTMLSelectElement).value)}>
          {workdirs.map((w) => (
            <option value={w.name}>{w.name}</option>
          ))}
          <option value={'\0new'}>Новая папка…</option>
        </select>
      </label>
      {(isNew || !workdirs.length) && (
        <label>
          Имя новой папки
          <input autoFocus required value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} pattern="[A-Za-z0-9_][A-Za-z0-9._\-]*" title="латиница, цифры, точка, дефис, подчёркивание" />
        </label>
      )}
      <div class="modal-actions">
        <button type="button" onClick={() => setState({ modal: null })}>
          Отмена
        </button>
        <button class="primary" type="submit" disabled={busy}>
          Начать
        </button>
      </div>
    </form>
  )
}

function DeviceLink({ url }: { url: string }) {
  return (
    <>
      <h3>Добавить устройство</h3>
      <p>Откройте ссылку на новом устройстве в течение 15 минут и зарегистрируйте там passkey.</p>
      <input class="link" readOnly value={url} onFocus={(e) => (e.target as HTMLInputElement).select()} />
      <div class="modal-actions">
        <button onClick={() => navigator.clipboard?.writeText(url).then(() => toast('Скопировано'))}>Скопировать</button>
      </div>
    </>
  )
}
