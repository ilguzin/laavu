import { useRef } from 'preact/hooks'
import { post, sessPath, upload } from '../api'
import { getState, openTab, tabId, toast, type SessTab } from '../store'
import { Icon } from '../icons'
import { leaveHistory, scrollBy, termOf } from './TermPane'

// Клавиши, которых нет на экранной клавиатуре телефона. PgUp и PgDn — не клавиши
// tmux, а прокрутка на страницу, как свайп.
const KEYS: [string, string][] = [
  ['PgUp', 'page:1'],
  ['PgDn', 'page:-1'],
  ['Esc', 'Escape'],
  ['Tab', 'Tab'],
  ['⇧Tab', 'BTab'],
  ['^C', 'C-c'],
  ['←', 'Left'],
  ['↑', 'Up'],
  ['↓', 'Down'],
  ['→', 'Right'],
  ['⏎', 'Enter'],
]

// insertText вставляет текст в терминал сессии без Enter (paste-buffer):
// путь загруженного файла, цитату из документа. Если сессия не на экране,
// её вкладка становится активной — дальше пользователь допишет вопрос.
export async function insertText(tab: SessTab, text: string) {
  const id = tabId(tab)
  if (!getState().groups.some((g) => g.tabs[g.active] && tabId(g.tabs[g.active]) === id)) openTab(tab)
  await leaveHistory(tab)
  try {
    await post(`${sessPath(tab.agent, tab.session)}/input`, { text, enter: false })
    termOf(tab)?.focus()
  } catch (e: any) {
    toast(e.message)
  }
}

// Загрузить файлы агенту в папку сессии и вставить пути в терминал.
export async function uploadTo(tab: SessTab, folder: string, files: FileList | File[] | null) {
  if (!files) return
  for (const f of Array.from(files)) {
    try {
      toast(`Загружаю ${f.name}…`)
      const r = await upload(tab.agent, folder, f, '', (p) => toast(`${f.name}: ${Math.round(p * 100)}%`))
      toast(`Загружено: ${r.path}`)
      await insertText(tab, r.path)
    } catch (e: any) {
      toast(e.message)
    }
  }
}

export async function sendKeys(tab: SessTab, keys: string[]) {
  await leaveHistory(tab)
  try {
    await post(`${sessPath(tab.agent, tab.session)}/keys`, { keys })
  } catch (e: any) {
    toast(e.message)
  }
}

export async function sendText(tab: SessTab, text: string, enter = true) {
  await leaveHistory(tab)
  try {
    await post(`${sessPath(tab.agent, tab.session)}/input`, { text, enter })
  } catch (e: any) {
    toast(e.message)
  }
}

function press(tab: SessTab, key: string) {
  if (!key.startsWith('page:')) return sendKeys(tab, [key])
  // Страница — высота терминала без двух строк, чтобы не терять нить.
  const rows = termOf(tab)?.rows || 24
  scrollBy(tab, Number(key.slice(5)) * Math.max(1, rows - 2))
}

// KeyBar — полоса клавиш под терминалом на телефоне, с файлом и камерой.
export function KeyBar({ tab, folder }: { tab: SessTab; folder: string }) {
  const fileRef = useRef<HTMLInputElement>(null)
  const camRef = useRef<HTMLInputElement>(null)
  return (
    <div class="keybar">
      <button class="ib" aria-label="Файл" title="Загрузить файл агенту" onClick={() => fileRef.current?.click()} disabled={!folder}>
        <Icon name="clip" size={18} />
      </button>
      <button class="ib" aria-label="Камера" title="Снять камерой" onClick={() => camRef.current?.click()} disabled={!folder}>
        <Icon name="camera" size={18} />
      </button>
      <div class="keys">
        {KEYS.map(([label, key]) => (
          <button class="key" onMouseDown={(e) => e.preventDefault()} onClick={() => press(tab, key)} title={key}>
            {label}
          </button>
        ))}
      </div>
      <input ref={fileRef} type="file" multiple hidden onChange={(e) => uploadTo(tab, folder, (e.target as HTMLInputElement).files)} />
      <input ref={camRef} type="file" accept="image/*" capture="environment" hidden onChange={(e) => uploadTo(tab, folder, (e.target as HTMLInputElement).files)} />
    </div>
  )
}
