// The worker is what shows a notification: a page that is not being looked at
// cannot show one of its own on every browser, and this can. It holds nothing
// of the conversation — the page hands it the words as it shows them.

self.addEventListener('install', () => self.skipWaiting())
self.addEventListener('activate', e => e.waitUntil(self.clients.claim()))

// A notification that was clicked opens the page it came from, or brings the
// one that is already open to the front.
self.addEventListener('notificationclick', e => {
  e.notification.close()
  e.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then(open => {
    for (const client of open) {
      if (client.url.startsWith(self.registration.scope) && 'focus' in client) return client.focus()
    }
    return self.clients.openWindow(self.registration.scope)
  }))
})
