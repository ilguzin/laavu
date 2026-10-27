// Splash before the bundle: the theme before the first paint (theme.ts does the same later)
// and the server address under the logo. A file, not inline: the CSP allows only 'self' scripts.
try {
  var t = localStorage.getItem('laavu.theme')
  if (t !== 'dark' && t !== 'light') t = matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  document.documentElement.dataset.theme = t
  if (t === 'light') document.querySelector('meta[name="theme-color"]').setAttribute('content', '#F3F4F1')
} catch (e) {}
document.addEventListener('readystatechange', function () {
  var h = document.querySelector('#splash .sp-host')
  if (h && !h.textContent) h.textContent = location.host
})
