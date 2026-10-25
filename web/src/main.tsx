import { render } from 'preact'
import '@fontsource/inter/400.css'
import '@fontsource/inter/500.css'
import '@fontsource/inter/600.css'
import '@fontsource/inter/700.css'
import '@fontsource/jetbrains-mono/400.css'
import '@fontsource/jetbrains-mono/700.css'
import './style.css'
import { initTheme } from './theme'
import { App } from './components/App'

initTheme()
render(<App />, document.getElementById('app')!)

if ('serviceWorker' in navigator && (location.protocol === 'https:' || location.hostname === 'localhost')) {
  navigator.serviceWorker.register('/sw.js', { scope: '/' }).catch(() => {})
}
