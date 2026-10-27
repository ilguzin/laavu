import type { ComponentChildren } from 'preact'
import { useEffect, useState } from 'preact/hooks'

// Popup menu at a button. anchor is the button's rect; the menu opens
// below it, or above if there is not enough room below.
export function Popover({ anchor, onClose, children, side = 'below' }: { anchor: DOMRect; onClose: () => void; children: ComponentChildren; side?: 'below' | 'right' }) {
  useEffect(() => {
    const f = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', f)
    return () => window.removeEventListener('keydown', f)
  }, [])
  const style: Record<string, string> = {}
  if (side === 'right') {
    style.left = `${anchor.right + 8}px`
    if (anchor.top > window.innerHeight / 2) style.bottom = `${window.innerHeight - anchor.bottom}px`
    else style.top = `${anchor.top}px`
  } else {
    if (anchor.bottom > window.innerHeight * 0.6) style.bottom = `${window.innerHeight - anchor.top + 6}px`
    else style.top = `${anchor.bottom + 6}px`
    if (anchor.left > window.innerWidth / 2) style.right = `${Math.max(8, window.innerWidth - anchor.right)}px`
    else style.left = `${Math.max(8, anchor.left)}px`
  }
  return (
    <>
      <div class="popover-bg" onClick={onClose} />
      <div class="popover" role="menu" style={style} onClick={onClose}>
        {children}
      </div>
    </>
  )
}

// usePopover: [anchor rect | null, open from an event, close].
export function usePopover(): [DOMRect | null, (e: Event) => void, () => void] {
  const [r, setR] = useState<DOMRect | null>(null)
  return [r, (e: Event) => setR((e.currentTarget as HTMLElement).getBoundingClientRect()), () => setR(null)]
}
