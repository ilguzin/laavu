// Theme: dark, light or system. Colors are tokens in style.css on
// :root[data-theme]; this file only picks one and builds the xterm theme from the same tokens.
import type { ITheme } from '@xterm/xterm'
import { lsGet, lsSet } from './api'

export type ThemePref = 'system' | 'dark' | 'light'

const query = window.matchMedia('(prefers-color-scheme: dark)')
const listeners = new Set<() => void>()

export function themePref(): ThemePref {
  const v = lsGet('laavu.theme')
  return v === 'dark' || v === 'light' ? v : 'system'
}

export function resolvedTheme(): 'dark' | 'light' {
  const p = themePref()
  return p === 'system' ? (query.matches ? 'dark' : 'light') : p
}

function apply() {
  document.documentElement.dataset.theme = resolvedTheme()
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', resolvedTheme() === 'dark' ? '#0E1311' : '#F3F4F1')
  for (const f of listeners) f()
}

export function setThemePref(p: ThemePref) {
  lsSet('laavu.theme', p)
  apply()
}

export function onThemeChange(f: () => void): () => void {
  listeners.add(f)
  return () => listeners.delete(f)
}

export function initTheme() {
  query.addEventListener('change', () => themePref() === 'system' && apply())
  apply()
}

// xterm theme from CSS variables: one color source for the page and the terminal.
export function termTheme(): ITheme {
  const cs = getComputedStyle(document.documentElement)
  const v = (n: string) => cs.getPropertyValue(n).trim()
  const dark = resolvedTheme() === 'dark'
  return {
    background: v('--term-bg'),
    foreground: v('--term-fg'),
    cursor: v('--term-fg'),
    cursorAccent: v('--term-bg'),
    selectionBackground: v('--term-sel'),
    black: dark ? '#1B2320' : '#18201C',
    brightBlack: dark ? '#5D6862' : '#5D6862',
    red: v('--bad'),
    brightRed: v('--bad'),
    green: v('--ok'),
    brightGreen: v('--ok'),
    yellow: dark ? '#E8B04A' : '#8A6100',
    brightYellow: dark ? '#F2C66D' : '#7A5600',
    blue: v('--work'),
    brightBlue: v('--work'),
    magenta: dark ? '#C79BE0' : '#8D4FAF',
    brightMagenta: dark ? '#D9B5EC' : '#7A3E9B',
    cyan: dark ? '#6FC9C0' : '#1C7F77',
    brightCyan: dark ? '#8ED8D0' : '#166A63',
    white: dark ? '#C9D1CB' : '#5D6862',
    brightWhite: dark ? '#E6E9E4' : '#18201C',
  }
}
