// Service worker. Reliability rules from CONVENTIONS §6 — read them before editing.
//
//  * The cache name carries the build hash, so a new version can never read an old
//    shell. The Go server substitutes __APP_VERSION__ when it serves this file, and
//    serves it with Cache-Control: no-cache.
//  * /api/ is network-first with a cache fallback: the last-seen calendar stays
//    readable offline, but a successful network response always wins.
//  * EVERY push shows a notification, including a generic fallback when the payload
//    is missing or unparseable. iOS revokes the subscription after roughly three
//    silent pushes — a silent push is a bug, never an optimisation.

const APP_VERSION = "__APP_VERSION__";
const CACHE = `almanack-${APP_VERSION}`;
const API_PREFIX = '/api/';

const SHELL = [
  '/',
  '/index.html',
  '/style.css',
  '/manifest.json',
  '/locales/fr.json',
  '/locales/en.json',
  '/icons/icon.svg',
  '/icons/maskable.svg',
  '/icons/icon-180.png',
  '/icons/icon-192.png',
  '/icons/icon-512.png',
  '/icons/maskable-512.png',
  '/js/app.js',
  '/js/api.js',
  '/js/colors.js',
  '/js/dates.js',
  '/js/dom.js',
  '/js/eventui.js',
  '/js/i18n.js',
  '/js/icons.js',
  '/js/images.js',
  '/js/push.js',
  '/js/router.js',
  '/js/state.js',
  '/js/ui.js',
  '/js/views/activity.js',
  '/js/views/agenda.js',
  '/js/views/auth.js',
  '/js/views/calendars.js',
  '/js/views/event.js',
  '/js/views/iosinstall.js',
  '/js/views/join.js',
  '/js/views/month.js',
  '/js/views/search.js',
  '/js/views/settings.js',
  '/js/views/week.js',
];

// ---------------------------------------------------------------------------
// Install / activate
// ---------------------------------------------------------------------------

self.addEventListener('install', (event) => {
  event.waitUntil((async () => {
    const cache = await caches.open(CACHE);
    // One failed asset must not void the whole precache.
    await Promise.all(SHELL.map((url) => cache.add(new Request(url, { cache: 'reload' })).catch(() => {})));
    await self.skipWaiting();
  })());
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    const names = await caches.keys();
    const stale = names.filter((n) => n.startsWith('almanack-') && n !== CACHE);
    await Promise.all(stale.map((n) => caches.delete(n)));
    await self.clients.claim();
    // Only an *upgrade* reloads open tabs; a first install has nothing to replace.
    if (stale.length) {
      const clients = await self.clients.matchAll({ type: 'window' });
      for (const client of clients) client.postMessage({ type: 'reload', version: APP_VERSION });
    }
  })());
});

self.addEventListener('message', (event) => {
  if (event.data && event.data.type === 'skipWaiting') self.skipWaiting();
});

// ---------------------------------------------------------------------------
// Fetch
// ---------------------------------------------------------------------------

/** Cached copies are tagged, and stripped of the version header so a stale body
 *  can never trigger the client's hard-reload handshake. */
async function taggedCacheResponse(cached) {
  const body = await cached.blob();
  const headers = new Headers(cached.headers);
  headers.set('X-From-Cache', '1');
  headers.delete('X-App-Version');
  return new Response(body, { status: cached.status, statusText: cached.statusText, headers });
}

function offlineError() {
  return new Response(JSON.stringify({ error: { code: 'network', message: 'offline' } }), {
    status: 503,
    headers: { 'Content-Type': 'application/json', 'X-From-Cache': '1' },
  });
}

async function networkFirst(request) {
  const cache = await caches.open(CACHE);
  try {
    const response = await fetch(request);
    if (response && response.ok && request.method === 'GET') {
      cache.put(request, response.clone()).catch(() => {});
    }
    return response;
  } catch (err) {
    const cached = await cache.match(request);
    if (cached) return taggedCacheResponse(cached);
    return offlineError();
  }
}

async function cacheFirst(request) {
  const cache = await caches.open(CACHE);
  const cached = await cache.match(request);
  if (cached) return cached;
  try {
    const response = await fetch(request);
    if (response && response.ok) cache.put(request, response.clone()).catch(() => {});
    return response;
  } catch (err) {
    if (request.mode === 'navigate') {
      const shell = await cache.match('/index.html') || await cache.match('/');
      if (shell) return shell;
    }
    throw err;
  }
}

self.addEventListener('fetch', (event) => {
  const request = event.request;
  if (request.method !== 'GET') return;

  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return;

  // sw.js itself and the dev routes are never cached.
  if (url.pathname === '/sw.js' || url.pathname.startsWith('/dev/')) return;

  if (url.pathname.startsWith(API_PREFIX)) {
    event.respondWith(networkFirst(request));
    return;
  }

  if (request.mode === 'navigate') {
    event.respondWith((async () => {
      try {
        return await fetch(request);
      } catch (err) {
        const cache = await caches.open(CACHE);
        return (await cache.match('/index.html')) || (await cache.match('/')) || offlineError();
      }
    })());
    return;
  }

  event.respondWith(cacheFirst(request));
});

// ---------------------------------------------------------------------------
// Push
// ---------------------------------------------------------------------------

const catalogs = new Map();

async function catalog(lang) {
  const want = lang === 'en' ? 'en' : 'fr';
  if (catalogs.has(want)) return catalogs.get(want);
  try {
    const cache = await caches.open(CACHE);
    const res = (await cache.match(`/locales/${want}.json`)) || (await fetch(`/locales/${want}.json`));
    const dict = await res.json();
    catalogs.set(want, dict);
    return dict;
  } catch (_) {
    return {};
  }
}

self.addEventListener('push', (event) => {
  // waitUntil is mandatory and unconditional: iOS counts pushes that display
  // nothing and revokes the subscription after about three of them.
  event.waitUntil(showPush(event));
});

async function showPush(event) {
  let payload = null;
  try {
    payload = event.data ? event.data.json() : null;
  } catch (_) {
    payload = null;
  }

  let title = payload && payload.title ? String(payload.title) : '';
  let body = payload && payload.body ? String(payload.body) : '';
  const url = payload && payload.url ? String(payload.url) : '/';
  const tag = payload && payload.tag ? String(payload.tag) : `almanack-${Date.now()}`;

  if (!title) {
    const dict = await catalog(payload && payload.lang);
    // 'Almanack' is the product name, identical in both catalogs; it is the last
    // resort if even the catalog is unreachable.
    title = dict['notify.fallback.title'] || 'Almanack';
    if (!body) body = dict['notify.fallback.body'] || '';
  }

  try {
    return await self.registration.showNotification(title, {
      body,
      tag,
      renotify: true,
      icon: '/icons/icon.svg',
      badge: '/icons/icon.svg',
      data: { url },
      timestamp: Date.now(),
    });
  } catch (_) {
    // Options a browser rejects must never cost us the notification itself.
    return self.registration.showNotification(title, { body, data: { url } });
  }
}

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const url = (event.notification.data && event.notification.data.url) || '/';
  event.waitUntil((async () => {
    const clients = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const client of clients) {
      try {
        await client.focus();
        client.postMessage({ type: 'navigate', url });
        if ('navigate' in client) await client.navigate(url);
        return;
      } catch (_) { /* try the next client */ }
    }
    await self.clients.openWindow(url);
  })());
});

// ---------------------------------------------------------------------------
// Subscription renewal
// ---------------------------------------------------------------------------

async function vapidKey() {
  try {
    const cache = await caches.open(CACHE);
    const res = (await cache.match('/api/v1/config')) || (await fetch('/api/v1/config'));
    const cfg = await res.json();
    return cfg.vapid_public_key || null;
  } catch (_) {
    return null;
  }
}

function base64UrlToUint8Array(base64) {
  const padded = String(base64).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(padded + '='.repeat((4 - (padded.length % 4)) % 4));
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

async function postSubscription(sub) {
  const json = sub.toJSON();
  await fetch('/api/v1/push/subscription', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'almanack' },
    body: JSON.stringify({
      endpoint: sub.endpoint,
      p256dh: json.keys && json.keys.p256dh,
      auth: json.keys && json.keys.auth,
      ua_label: 'PWA',
    }),
  });
}

self.addEventListener('pushsubscriptionchange', (event) => {
  event.waitUntil((async () => {
    let sub = event.newSubscription || null;
    if (!sub) {
      const key = (event.oldSubscription && event.oldSubscription.options
        && event.oldSubscription.options.applicationServerKey) || await vapidKey();
      if (!key) return;
      sub = await self.registration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: typeof key === 'string' ? base64UrlToUint8Array(key) : key,
      });
    }
    await postSubscription(sub);
  })().catch(() => { /* the client's liveness check repairs it on next open */ }));
});
