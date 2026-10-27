// Laavu service worker: push only. No offline cache: the client is useless without the server.
self.addEventListener('install', () => self.skipWaiting())
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()))

self.addEventListener('push', (e) => {
  let m = {}
  try {
    m = e.data ? e.data.json() : {}
  } catch {}
  const title = m.title || 'Laavu'
  e.waitUntil(
    self.registration.showNotification(title, {
      body: m.body || '',
      tag: m.tag || 'laavu',
      renotify: true,
      icon: '/icon-192.png',
      badge: '/icon-192.png',
      data: { agent: m.agent, session: m.session },
    }),
  )
})

self.addEventListener('notificationclick', (e) => {
  e.notification.close()
  const d = e.notification.data || {}
  const target = d.agent && d.session ? `/#/${encodeURIComponent(d.agent)}/${encodeURIComponent(d.session)}` : '/'
  e.waitUntil(
    (async () => {
      const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true })
      for (const c of all) {
        if (new URL(c.url).origin === self.location.origin) {
          await c.focus()
          if (d.agent) c.postMessage({ type: 'open', agent: d.agent, session: d.session })
          return
        }
      }
      await self.clients.openWindow(target)
    })(),
  )
})
