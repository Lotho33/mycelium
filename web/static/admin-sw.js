// Minimal service worker for the admin dashboard PWA (installed at
// /admin/sw.js, not /static/admin-sw.js — see internal/api/admin_pwa.go's
// doc comment for why the URL matters: a worker's default max scope is the
// directory of its own script).
//
// Deliberately does NOT cache anything: the dashboard is a live view over
// the server's own state (logs, plugin status, resource usage, …) — an
// offline-first cache would serve stale data indistinguishable from real
// data, which is worse than no offline support at all for an admin tool.
// This exists purely to satisfy Chrome's PWA installability criterion of
// "a registered service worker with a fetch handler"; every request still
// goes straight to the network, exactly as if there were no service worker.

self.addEventListener('install', (event) => {
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener('fetch', (event) => {
  event.respondWith(fetch(event.request));
});
