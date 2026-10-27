import { useEffect, useRef, useState } from 'preact/hooks'
import { Icon } from '../icons'
import {
  closeTab,
  focusTab,
  MAX_GROUPS,
  mergeGroups,
  moveTab,
  pinTab,
  sessionOf,
  setState,
  tabId,
  tabKey,
  useMobile,
  useStore,
  type Group,
  type Tab,
} from '../store'
import { socket } from '../ws'
import { confirmClose, FileView } from './FileView'
import { Popover, usePopover } from './Popover'
import { SessionMenuItems, SessionView } from './SessionView'
import { Dot, statusLabel } from './Status'

function tabLabel(t: Tab) {
  return t.kind === 'session' ? t.session : t.path.split('/').pop() || t.path
}

export function Workspace() {
  const groups = useStore((s) => s.groups)
  const focus = useStore((s) => s.focus)
  const screen = useStore((s) => s.mobileScreen)
  useStore((s) => s.sessions)
  const mobile = useMobile()

  // Which sessions are on screen: tells the daemon not to push about what is already visible.
  const shown = mobile && screen !== 'work' ? [] : mobile ? [groups[focus]] : groups
  const focusKeys = shown
    .map((g) => g?.tabs[g.active])
    .filter((t): t is Tab => !!t && t.kind === 'session')
    .map((t) => tabKey(t as any))
  useEffect(() => {
    socket.setFocus(focusKeys)
  }, [focusKeys.join(',')])

  const n = groups.length
  return (
    <section class="work">
      {mobile && <MobileHead />}
      <div class={'groups n' + n}>
        {groups.map((g, gi) => (
          <GroupView key={gi} g={g} gi={gi} focused={gi === focus} total={n} paneVisible={!mobile || (screen === 'work' && gi === focus)} />
        ))}
      </div>
    </section>
  )
}

function GroupView({ g, gi, focused, total, paneVisible }: { g: Group; gi: number; focused: boolean; total: number; paneVisible: boolean }) {
  const dirty = useStore((s) => s.dirty)
  const [dropAt, setDropAt] = useState(-1)
  const strip = useRef<HTMLDivElement>(null)
  const cur = g.tabs[g.active]

  function onDrop(e: DragEvent) {
    e.preventDefault()
    setDropAt(-1)
    const d = e.dataTransfer?.getData('application/x-laavu-tab')
    if (!d) return
    const [from, i] = d.split(':').map(Number)
    if (from !== gi) moveTab(from, i, gi)
  }

  return (
    <div class={'group' + (focused ? ' focused' : '')} data-group={gi} onMouseDown={() => !focused && focusTab(gi, g.active)}>
      <div
        class="tabstrip"
        ref={strip}
        onDragOver={(e) => {
          if (e.dataTransfer?.types.includes('application/x-laavu-tab')) {
            e.preventDefault()
            setDropAt(gi)
          }
        }}
        onDragLeave={() => setDropAt(-1)}
        onDrop={onDrop}
        style={dropAt === gi ? { boxShadow: 'inset 0 -2px 0 var(--ember)' } : undefined}
      >
        <div class="tabs" role="tablist">
          {g.tabs.map((t, i) => {
            const id = tabId(t)
            const s = sessionOf(t)
            return (
              <div
                class={'tab ' + t.kind + (i === g.active ? ' on' : '') + (t.kind === 'file' && t.preview ? ' preview' : '') + (dirty[id] ? ' is-dirty' : '')}
                role="tab"
                aria-selected={i === g.active}
                draggable
                onDragStart={(e) => e.dataTransfer?.setData('application/x-laavu-tab', `${gi}:${i}`)}
                onClick={() => focusTab(gi, i)}
                onDblClick={() => t.kind === 'file' && pinTab(id)}
                onAuxClick={(e) => e.button === 1 && confirmClose(id) && closeTab(gi, i)}
                data-tab={t.kind === 'session' ? tabKey(t) : undefined}
                data-file={t.kind === 'file' ? t.path : undefined}
                title={t.kind === 'file' ? `${t.folder}/${t.path}` : `${t.session} · ${statusLabel(s?.status)}`}
              >
                {t.kind === 'session' ? s?.kind === 'claude' ? <Dot status={s?.status} /> : <Icon name="terminal" size={13} /> : <Icon name="file" size={13} />}
                <span class="label">{tabLabel(t)}</span>
                {dirty[id] && <span class="dirty" title="Unsaved" />}
                <button
                  class="x"
                  onClick={(e) => {
                    e.stopPropagation()
                    if (confirmClose(id)) closeTab(gi, i)
                  }}
                  aria-label="close tab"
                >
                  <Icon name="x" size={12} stroke={2} />
                </button>
              </div>
            )
          })}
        </div>
        <div class="strip-acts">
          {g.tabs.length > 1 && total < MAX_GROUPS && (
            <button class="ib sm" title="Open to the side" aria-label="Open to the side" onClick={() => moveTab(gi, g.active, total)}>
              <Icon name="split" size={14} />
            </button>
          )}
          {total > 1 && gi === total - 1 && (
            <button class="ib sm" title="Merge all tabs into one group" aria-label="Single group" onClick={mergeGroups}>
              <Icon name="single" size={14} />
            </button>
          )}
        </div>
      </div>
      <div class="group-body">
        {g.tabs.map((t, i) => {
          const visible = paneVisible && i === g.active
          return (
            <div class={'pane' + (i === g.active ? ' shown' : '')} key={tabId(t)}>
              {t.kind === 'session' ? (
                <SessionView tab={t} session={sessionOf(t)} visible={visible} focused={focused && visible} onFocus={() => focusTab(gi, i)} />
              ) : (
                <FileView tab={t} visible={visible} />
              )}
            </div>
          )
        })}
        {!cur && (
          <div class="empty-work">
            <Icon name="logo" size={44} stroke={1.4} />
            <div>Pick a session on the left or start a new one.</div>
            <button class="primary" onClick={() => setState({ modal: { kind: 'newSession' } })}>
              <Icon name="plus" size={15} stroke={2.4} />
              New session
            </button>
          </div>
        )}
      </div>
    </div>
  )
}

// Tab screen header on a phone: back to the list, name, status, menu.
function MobileHead() {
  const groups = useStore((s) => s.groups)
  const focus = useStore((s) => s.focus)
  useStore((s) => s.sessions)
  const ctx = useStore((s) => s.context)
  const [menu, openMenu, closeMenu] = usePopover()
  const g = groups[focus]
  const t = g?.tabs[g.active]
  if (!t) return null
  const s = sessionOf(t)
  const c = t.kind === 'session' ? ctx[tabKey(t)] : undefined
  const sub = t.kind === 'session' ? [statusLabel(s?.status), c && s?.kind === 'claude' ? `context ${c.percent}%` : ''].filter(Boolean).join(' · ') : `${t.folder}/${t.path}`
  return (
    <div class="mob-head">
      <button class="ib" aria-label="Back to sessions" onClick={() => setState({ mobileScreen: 'sessions' })}>
        <Icon name="back" size={22} stroke={2} />
      </button>
      <div class="title">
        <b>{tabLabel(t)}</b>
        <span style={s?.status === 'waiting' ? 'color: var(--ember); font-weight: 600' : ''}>{sub}</span>
      </div>
      {g.tabs.length > 1 && (
        <span class="muted small">
          {g.active + 1}/{g.tabs.length}
        </span>
      )}
      {t.kind === 'session' && (
        <button class="ib" aria-label="Session menu" onClick={openMenu}>
          <Icon name="more" size={20} />
        </button>
      )}
      {t.kind === 'file' && (
        <button class="ib" aria-label="Close file" onClick={() => {
            closeTab(focus, g.active)
            setState({ mobileScreen: 'files' })
          }}>
          <Icon name="x" size={20} />
        </button>
      )}
      {menu && t.kind === 'session' && (
        <Popover anchor={menu} onClose={closeMenu}>
          <SessionMenuItems tab={t} session={s} />
        </Popover>
      )}
    </div>
  )
}
