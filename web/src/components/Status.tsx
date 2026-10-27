const labels: Record<string, string> = {
  starting: 'starting',
  working: 'working',
  waiting: 'waiting for reply',
  sleeping: 'sleeping',
  exited: 'exited',
  error: 'error',
  active: 'output',
  quiet: 'quiet',
}

export const statusLabel = (s?: string) => (s ? labels[s] || s : '')

export function Dot({ status }: { status?: string }) {
  return <span class={'dot st-' + (status || 'none')} title={statusLabel(status)} aria-label={statusLabel(status)} />
}
