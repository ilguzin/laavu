import type { ComponentChildren } from 'preact'
import { useEffect, useState } from 'preact/hooks'

// Всплывающее меню у кнопки. anchor — прямоугольник кнопки; меню открывается
// под ней или над ней, если снизу не хватает места.
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

// usePopover: [прямоугольник якоря | null, открыть по событию, закрыть].
export function usePopover(): [DOMRect | null, (e: Event) => void, () => void] {
  const [r, setR] = useState<DOMRect | null>(null)
  return [r, (e: Event) => setR((e.currentTarget as HTMLElement).getBoundingClientRect()), () => setR(null)]
}
