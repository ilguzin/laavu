const labels: Record<string, string> = {
  starting: 'запускается',
  working: 'работает',
  waiting: 'ждёт ответа',
  sleeping: 'спит',
  exited: 'завершена',
  error: 'ошибка',
  active: 'выводит',
  quiet: 'тихо',
}

export const statusLabel = (s?: string) => (s ? labels[s] || s : '')

export function Dot({ status }: { status?: string }) {
  return <span class={'dot st-' + (status || 'none')} title={statusLabel(status)} aria-label={statusLabel(status)} />
}
