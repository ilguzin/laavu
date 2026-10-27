// Source highlighting with line numbers. Loaded lazily.
import hljs from 'highlight.js/lib/common'

const extLang: Record<string, string> = {
  md: 'markdown', markdown: 'markdown', yml: 'yaml', tsx: 'typescript', jsx: 'javascript', mjs: 'javascript',
  h: 'c', hpp: 'cpp', cc: 'cpp', sh: 'bash', zsh: 'bash', htm: 'html', svg: 'xml', toml: 'ini', conf: 'ini',
}

export function highlight(code: string, path: string): string[] {
  const ext = (path.split('.').pop() || '').toLowerCase()
  const lang = extLang[ext] || ext
  let html: string
  if (code.length > 400_000) html = escape(code)
  else if (hljs.getLanguage(lang)) html = hljs.highlight(code, { language: lang, ignoreIllegals: true }).value
  else html = escape(code)
  return splitLines(html)
}

function escape(s: string) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

// Split highlighted HTML into lines, closing and reopening spans.
function splitLines(html: string): string[] {
  const lines: string[] = []
  const open: string[] = []
  let cur = ''
  const re = /(<span[^>]*>)|(<\/span>)|(\n)|([^<\n]+|<)/g
  let m: RegExpExecArray | null
  while ((m = re.exec(html))) {
    if (m[1]) {
      open.push(m[1])
      cur += m[1]
    } else if (m[2]) {
      open.pop()
      cur += m[2]
    } else if (m[3]) {
      lines.push(cur + '</span>'.repeat(open.length))
      cur = open.join('')
    } else cur += m[4]
  }
  lines.push(cur + '</span>'.repeat(open.length))
  return lines
}
