// Редактор на CodeMirror 6. Грузится отдельным чанком при первой правке.
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { markdown, markdownLanguage } from '@codemirror/lang-markdown'
import { bracketMatching, HighlightStyle, indentOnInput, LanguageDescription, syntaxHighlighting } from '@codemirror/language'
import { languages } from '@codemirror/language-data'
import { unifiedMergeView } from '@codemirror/merge'
import { highlightSelectionMatches, search, searchKeymap } from '@codemirror/search'
import { Compartment, EditorState } from '@codemirror/state'
import { drawSelection, EditorView, highlightActiveLine, highlightActiveLineGutter, keymap, lineNumbers } from '@codemirror/view'
import { tags as t } from '@lezer/highlight'

// Цвета — те же CSS-переменные, что у страницы: смена темы не требует пересоздания.
const theme = EditorView.theme({
  '&': { height: '100%', backgroundColor: 'var(--bg1)', color: 'var(--fg)', fontSize: '13.5px' },
  '.cm-scroller': { fontFamily: "'JetBrains Mono', ui-monospace, monospace", lineHeight: '1.6' },
  '.cm-content': { caretColor: 'var(--fg)', padding: '10px 0' },
  '.cm-cursor': { borderLeftColor: 'var(--fg)', borderLeftWidth: '2px' },
  '&.cm-focused': { outline: 'none' },
  '.cm-gutters': { backgroundColor: 'var(--bg1)', color: 'var(--faint)', border: 'none', paddingLeft: '6px' },
  '.cm-activeLineGutter': { backgroundColor: 'transparent', color: 'var(--muted)' },
  '.cm-activeLine': { backgroundColor: 'color-mix(in srgb, var(--work) 6%, transparent)' },
  '&.cm-focused .cm-selectionBackground, .cm-selectionBackground, ::selection': { backgroundColor: 'var(--term-sel) !important' },
  '.cm-selectionMatch': { backgroundColor: 'color-mix(in srgb, var(--ember) 18%, transparent)' },
  '.cm-matchingBracket': { backgroundColor: 'var(--bg3)', outline: 'none' },
  '.cm-panels': { backgroundColor: 'var(--bg2)', color: 'var(--fg)', borderColor: 'var(--line)' },
  '.cm-panel.cm-search input, .cm-panel.cm-search button': { fontSize: '13px' },
  '.cm-searchMatch': { backgroundColor: 'color-mix(in srgb, var(--ember) 25%, transparent)' },
  '.cm-searchMatch-selected': { backgroundColor: 'color-mix(in srgb, var(--ember) 45%, transparent)' },
  // Сравнение с версией на диске.
  '.cm-changedLine': { backgroundColor: 'color-mix(in srgb, var(--ok) 10%, transparent) !important' },
  '.cm-changedText': { background: 'color-mix(in srgb, var(--ok) 30%, transparent) !important' },
  '.cm-deletedChunk': { backgroundColor: 'color-mix(in srgb, var(--bad) 10%, transparent)', paddingLeft: '6px' },
  '.cm-deletedChunk del': { textDecoration: 'none', color: 'var(--bad)' },
  '.cm-deletedChunk .cm-chunkButtons': { display: 'flex', gap: '6px', top: '2px' },
  '.cm-deletedChunk .cm-chunkButtons button': {
    height: '24px',
    fontSize: '12px',
    padding: '0 8px',
    color: 'var(--fg)',
    background: 'var(--bg1)',
    border: '1px solid var(--line)',
    borderRadius: '6px',
    fontFamily: 'Inter, system-ui, sans-serif',
  },
})

const highlight = HighlightStyle.define([
  { tag: [t.keyword, t.modifier, t.operatorKeyword, t.controlKeyword], color: 'var(--work)' },
  { tag: [t.string, t.special(t.string), t.regexp], color: 'var(--ok)' },
  { tag: [t.number, t.bool, t.null, t.atom], color: 'var(--ember)' },
  { tag: [t.comment, t.meta], color: 'var(--faint)', fontStyle: 'italic' },
  { tag: [t.function(t.variableName), t.function(t.propertyName), t.definition(t.function(t.variableName))], color: 'var(--fg)', fontWeight: '600' },
  { tag: [t.typeName, t.className, t.propertyName, t.attributeName], color: 'var(--type, #c79be0)' },
  { tag: t.heading, color: 'var(--work)', fontWeight: '700' },
  { tag: t.heading1, fontSize: '1.25em' },
  { tag: t.heading2, fontSize: '1.12em' },
  { tag: t.strong, fontWeight: '700' },
  { tag: t.emphasis, fontStyle: 'italic' },
  { tag: t.strikethrough, textDecoration: 'line-through', color: 'var(--muted)' },
  { tag: [t.link, t.url], color: 'var(--ember)' },
  { tag: t.monospace, color: 'var(--ok)' },
  { tag: [t.processingInstruction, t.contentSeparator, t.list, t.quote], color: 'var(--muted)' },
])

export interface EditorOptions {
  doc: string
  path: string
  onChange: (doc: string) => void
  onSave: () => void
}

export interface EditorHandle {
  getDoc(): string
  setDoc(text: string): void
  // Сравнение с текстом на диске; null — выключить.
  setCompare(original: string | null): void
  focus(): void
  destroy(): void
}

function mergeButton(type: 'reject' | 'accept', action: (e: MouseEvent) => void): HTMLElement {
  const b = document.createElement('button')
  b.textContent = type === 'accept' ? 'Оставить моё' : 'Взять с диска'
  b.onmousedown = action
  return b
}

export function createEditor(parent: HTMLElement, o: EditorOptions): EditorHandle {
  const lang = new Compartment()
  const compare = new Compartment()
  let silent = false
  const view = new EditorView({
    parent,
    state: EditorState.create({
      doc: o.doc,
      extensions: [
        lineNumbers(),
        highlightActiveLineGutter(),
        highlightActiveLine(),
        history(),
        drawSelection(),
        indentOnInput(),
        bracketMatching(),
        highlightSelectionMatches(),
        search({ top: true }),
        EditorView.lineWrapping,
        keymap.of([{ key: 'Mod-s', preventDefault: true, run: () => (o.onSave(), true) }, indentWithTab, ...defaultKeymap, ...historyKeymap, ...searchKeymap]),
        theme,
        syntaxHighlighting(highlight),
        lang.of([]),
        compare.of([]),
        EditorView.updateListener.of((u) => {
          if (u.docChanged && !silent) o.onChange(u.state.doc.toString())
        }),
      ],
    }),
  })
  // Язык — по имени файла; описания языков грузятся по требованию.
  const desc = LanguageDescription.matchFilename(languages, o.path)
  if (/\.(md|markdown|mdx)$/i.test(o.path)) {
    view.dispatch({ effects: lang.reconfigure(markdown({ base: markdownLanguage, codeLanguages: languages })) })
  } else if (desc) {
    desc.load().then((l) => view.dispatch({ effects: lang.reconfigure(l) })).catch(() => {})
  }
  return {
    getDoc: () => view.state.doc.toString(),
    setDoc(text) {
      if (text === view.state.doc.toString()) return
      silent = true
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text } })
      silent = false
    },
    setCompare(original) {
      view.dispatch({ effects: compare.reconfigure(original === null ? [] : unifiedMergeView({ original, mergeControls: mergeButton, syntaxHighlightDeletions: false })) })
    },
    focus: () => view.focus(),
    destroy: () => view.destroy(),
  }
}
