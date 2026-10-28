// Raw HTML in Markdown, reduced to a few harmless tags. Markdown renders on the
// app's origin, so a tag with attributes could run script there: only the tags
// below pass, without attributes (`details` keeps `open`); comments are dropped
// and anything else is shown as text.
const allowed = new Set(['details', 'summary', 'b', 'strong', 'i', 'em', 'br', 'kbd', 'sub', 'sup'])

const tag = /<!--[\s\S]*?(?:-->|$)|<(\/?)([a-zA-Z][a-zA-Z0-9-]*)([^<>]*)>/g

const text = (s: string) => s.replace(/</g, '&lt;').replace(/>/g, '&gt;')

export function safeHtml(src: string): string {
  let out = ''
  let last = 0
  for (const m of src.matchAll(tag)) {
    out += text(src.slice(last, m.index))
    last = m.index! + m[0].length
    if (m[0].startsWith('<!--')) continue
    const [, close, name, attrs] = m
    const n = name.toLowerCase()
    if (!allowed.has(n)) out += text(m[0])
    else if (close) out += `</${n}>`
    else out += n === 'details' && /(^|\s)open(\s|=|\/|$)/i.test(attrs) ? '<details open>' : `<${n}>`
  }
  return out + text(src.slice(last))
}
