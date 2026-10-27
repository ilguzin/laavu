import { post } from '../api'
import { Icon } from '../icons'
import { subscribePush } from '../push'
import { setState, toast, useStore } from '../store'
import { setThemePref, themePref, type ThemePref } from '../theme'
import { Popover, usePopover } from './Popover'
import { useState } from 'preact/hooks'

export const initials = (name: string) => name.replace(/[^\p{L}\p{N}]/gu, '').slice(0, 2).toLowerCase() || '?'

// User menu items: in the rail on desktop and under "More" on a phone.
export function UserMenuItems() {
  const me = useStore((s) => s.me)
  const [, force] = useState(0)
  const pref = themePref()
  const next: Record<ThemePref, ThemePref> = { system: 'dark', dark: 'light', light: 'system' }
  const label: Record<ThemePref, string> = { system: 'system', dark: 'dark', light: 'light' }

  async function deviceLink() {
    try {
      const r = await post('/api/auth/device-link')
      setState({ modal: { kind: 'device', data: r.url } })
    } catch (e: any) {
      toast(e.message)
    }
  }
  async function logout() {
    await post('/api/auth/logout').catch(() => {})
    location.reload()
  }

  return (
    <>
      <div class="info">
        <b style="color: var(--fg)">{me?.name}</b>
      </div>
      {me?.role === 'admin' && me.isolation !== undefined && (
        <div class={'info' + (me.isolation?.failed ? ' warn' : '')} data-testid="isolation">
          {me.isolation
            ? `Agent isolation: ${new Date(me.isolation.time * 1000).toLocaleString('en')}, ${me.isolation.failed ? `open probes: ${me.isolation.failed}` : 'all closed'}`
            : 'Agent isolation not checked yet'}
        </div>
      )}
      <hr />
      <button
        onClick={(e) => {
          e.stopPropagation()
          setThemePref(next[pref])
          force((x) => x + 1)
        }}
      >
        <Icon name={pref === 'dark' ? 'moon' : pref === 'light' ? 'sun' : 'monitor'} />
        Theme: {label[pref]}
      </button>
      {me?.push && !me.no_auth && (
        <button onClick={() => subscribePush().then(toast, (e) => toast(e.message))}>
          <Icon name="bell" />
          Notifications on this device
        </button>
      )}
      {!me?.no_auth && (
        <button onClick={deviceLink}>
          <Icon name="plus" />
          Add device
        </button>
      )}
      {!me?.no_auth && (
        <button onClick={logout}>
          <Icon name="back" />
          Sign out
        </button>
      )}
    </>
  )
}

export function Rail() {
  const agents = useStore((s) => s.agents)
  const agent = useStore((s) => s.agent)
  const sessions = useStore((s) => s.sessions)
  const me = useStore((s) => s.me)
  const connected = useStore((s) => s.connected)
  const sideOpen = useStore((s) => s.sideOpen)
  const navOpen = useStore((s) => s.navOpen)
  const [menu, openMenu, closeMenu] = usePopover()

  return (
    <nav class="rail" aria-label="Agents">
      <span class="logo" title="Laavu">
        <Icon name="logo" size={28} stroke={2.2} />
      </span>
      {agents.map((a) => {
        const waiting = (sessions[a.name] || []).filter((s) => s.status === 'waiting').length
        return (
          <button
            class={'rail-agent' + (a.name === agent ? ' on' : '')}
            title={a.name + (waiting ? ` · waiting for reply: ${waiting}` : '')}
            aria-label={a.name}
            data-agent={a.name}
            onClick={() => setState({ agent: a.name, sideOpen: true })}
          >
            {initials(a.name)}
            {waiting > 0 && <span class="badge">{waiting}</span>}
          </button>
        )
      })}
      <span class="rail-sep" />
      <button class={'ib' + (sideOpen ? ' on' : '')} title="Session list" aria-label="Session list" onClick={() => setState({ sideOpen: !sideOpen })}>
        <Icon name="sidebar" size={18} />
      </button>
      <button class={'ib' + (navOpen ? ' on' : '')} title="Files" aria-label="Files panel" onClick={() => setState({ navOpen: !navOpen })}>
        <Icon name="panelRight" size={18} />
      </button>
      <span class="spacer" />
      <button class="me" aria-label="Menu" title={connected ? 'connected' : 'offline, reconnecting'} onClick={openMenu}>
        {initials(me?.name || '?').slice(0, 1).toUpperCase()}
        <span class={'conn' + (connected ? '' : ' off')} />
      </button>
      {menu && (
        <Popover anchor={menu} onClose={closeMenu} side="right">
          <UserMenuItems />
        </Popover>
      )}
    </nav>
  )
}
