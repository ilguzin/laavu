import { useEffect, useRef } from 'preact/hooks'
import { get, post, sessPath, type SessionInfo } from '../api'
import { Icon } from '../icons'
import { closeSessionTabs, setState, tabKey, toast, useMobile, useStore, type SessTab } from '../store'
import { KeyBar, sendText, uploadTo } from './KeyBar'
import { Popover, usePopover } from './Popover'
import { statusLabel } from './Status'
import { TermPane } from './TermPane'

export async function openPort(agent: string) {
  const p = prompt("Port of the agent's dev server")
  if (!p) return
  try {
    const r = await post(`/api/agents/${encodeURIComponent(agent)}/ports/${encodeURIComponent(p.trim())}/open`)
    window.open(r.url, '_blank', 'noopener')
  } catch (e: any) {
    toast(e.message)
  }
}

// Session menu items: in the header on desktop and under "⋯" on a phone.
export function SessionMenuItems({ tab, session }: { tab: SessTab; session?: SessionInfo }) {
  const isClaude = session?.kind === 'claude'
  const waiting = session?.status === 'waiting'
  return (
    <>
      {isClaude && (
        <>
          <button disabled={!waiting} title={waiting ? '' : 'Interrupt the turn first (Esc)'} onClick={() => sendText(tab, '/compact')}>
            Compact context
          </button>
          <button disabled={!waiting} onClick={() => confirm('Clear the session context? The conversation stays in Claude Code history.') && sendText(tab, '/clear')}>
            Clear context
          </button>
          <hr />
        </>
      )}
      <button onClick={() => setState({ modal: { kind: 'history', data: tab } })}>
        <Icon name="history" />
        History
      </button>
      <button onClick={() => openPort(tab.agent)}>
        <Icon name="globe" />
        Open port
      </button>
      <hr />
      <button
        onClick={() => {
          if (!confirm(`Close session ${tab.session}? Its process will exit.`)) return
          post(`${sessPath(tab.agent, tab.session)}/kill`)
            .then(() => closeSessionTabs(tab.agent, tab.session))
            .catch((e) => toast(e.message))
        }}
      >
        <Icon name="x" />
        Close session
      </button>
    </>
  )
}

interface Props {
  tab: SessTab
  session?: SessionInfo
  visible: boolean
  focused: boolean
  onFocus: () => void
}

export function SessionView({ tab, session, visible, focused, onFocus }: Props) {
  const k = tabKey(tab)
  const ctx = useStore((s) => s.context[k])
  const mobile = useMobile()
  const [menu, openMenu, closeMenu] = usePopover()
  const fileRef = useRef<HTMLInputElement>(null)
  const isClaude = session?.kind === 'claude'
  const status = session?.status || ''

  useEffect(() => {
    if (!isClaude || session?.sleeping || !visible) return
    get(`${sessPath(tab.agent, tab.session)}/context`)
      .then((c) => setState((s) => ({ context: { ...s.context, [k]: c } })))
      .catch(() => {})
  }, [k, isClaude, session?.claude, visible])

  const folder = session?.folder || ''
  return (
    <>
      <div class="sess-head">
        <span class="path" title={session?.path}>
          {folder ? `~/${folder}` : tab.session}
        </span>
        {status && (
          <span class={'pill st-' + status}>
            <span class={'dot st-' + status} />
            {statusLabel(status)}
          </span>
        )}
        <span class="spacer" />
        {ctx && isClaude && (
          <span class={'ctx' + (ctx.percent >= 65 ? ' warn' : '')} title={`${ctx.used.toLocaleString('en')} of ${ctx.window.toLocaleString('en')} tokens`}>
            context
            <span class="bar">
              <i style={{ width: `${Math.min(100, ctx.percent)}%` }} />
            </span>
            {ctx.percent}%
          </span>
        )}
        <button class="ib" title="Upload a file for the agent" aria-label="Upload file" disabled={!folder} onClick={() => fileRef.current?.click()}>
          <Icon name="clip" size={17} />
        </button>
        <button class="ib" title="Terminal history" aria-label="History" onClick={() => setState({ modal: { kind: 'history', data: tab } })}>
          <Icon name="history" size={17} />
        </button>
        <button class="ib" title="More" aria-label="Session menu" onClick={openMenu}>
          <Icon name="more" size={17} />
        </button>
        <input ref={fileRef} type="file" multiple hidden onChange={(e) => uploadTo(tab, folder, (e.target as HTMLInputElement).files)} />
      </div>
      <TermPane tab={tab} visible={visible} focused={focused} onFocus={onFocus} />
      {session?.dead ? (
        <div class="exited-bar">
          <div class="card">
            <span>The process in session {tab.session} has exited.</span>
            <span class="spacer" />
            <button class="primary" onClick={() => post(`${sessPath(tab.agent, tab.session)}/respawn`).catch((e) => toast(e.message))}>
              Resume
            </button>
            <button
              onClick={() =>
                post(`${sessPath(tab.agent, tab.session)}/kill`)
                  .then(() => closeSessionTabs(tab.agent, tab.session))
                  .catch((e) => toast(e.message))
              }
            >
              Close
            </button>
          </div>
        </div>
      ) : (
        mobile && <KeyBar tab={tab} folder={folder} />
      )}
      {menu && (
        <Popover anchor={menu} onClose={closeMenu}>
          <SessionMenuItems tab={tab} session={session} />
        </Popover>
      )}
    </>
  )
}
