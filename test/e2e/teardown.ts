import { execSync } from 'node:child_process'
import { existsSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

// Сервер tmux переживает laavud dev — гасим его сами.
export default function () {
  const here = dirname(fileURLToPath(import.meta.url))
  for (const port of [7700, 7800]) {
    const dir = join(here, `.state-${port}`, 'tmux')
    if (existsSync(dir)) {
      try {
        execSync('tmux -L laavu-dev kill-server', { env: { ...process.env, TMUX_TMPDIR: dir }, stdio: 'ignore' })
      } catch {}
    }
  }
}
