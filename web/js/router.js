// Hash router. Hash routing (not History API) is deliberate: the server can serve
// index.html for '/' alone, and a push payload URL like '/#/event/12/2026-08-04'
// works whether the app is open, backgrounded, or cold.

const routes = [];
let fallback = null;
let currentMatch = { path: '/', params: {}, query: new URLSearchParams() };
let started = false;
let depth = 0;

/** route('/event/:id/:date', handler) — ':name' captures one segment. */
export function route(pattern, handler) {
  const parts = pattern.split('/').filter(Boolean);
  routes.push({ parts, handler, pattern });
}

export function notFound(handler) {
  fallback = handler;
}

function parseHash() {
  const raw = location.hash.replace(/^#/, '') || '/';
  const qi = raw.indexOf('?');
  const path = qi === -1 ? raw : raw.slice(0, qi);
  const query = new URLSearchParams(qi === -1 ? '' : raw.slice(qi + 1));
  return { path: path.startsWith('/') ? path : `/${path}`, query };
}

function match(path) {
  const segs = path.split('/').filter(Boolean);
  for (const r of routes) {
    if (r.parts.length !== segs.length) continue;
    const params = {};
    let ok = true;
    for (let i = 0; i < r.parts.length; i++) {
      const p = r.parts[i];
      if (p.startsWith(':')) params[p.slice(1)] = decodeURIComponent(segs[i]);
      else if (p !== segs[i]) { ok = false; break; }
    }
    if (ok) return { route: r, params };
  }
  return null;
}

function dispatch() {
  const { path, query } = parseHash();
  const found = match(path);
  currentMatch = { path, params: found ? found.params : {}, query };
  const handler = found ? found.route.handler : fallback;
  if (handler) handler(currentMatch);
}

export function start() {
  if (started) return;
  started = true;
  window.addEventListener('hashchange', () => {
    depth++;
    dispatch();
  });
  dispatch();
}

/** Re-run the current route (used after a mutation invalidates the screen). */
export function reload() {
  dispatch();
}

export function current() {
  return currentMatch;
}

export function go(path, { replace = false } = {}) {
  const target = path.startsWith('#') ? path : `#${path}`;
  if (location.hash === target) {
    dispatch();
    return;
  }
  if (replace) {
    const url = location.pathname + location.search + target;
    history.replaceState(null, '', url);
    dispatch();
  } else {
    location.hash = target;
  }
}

/** Go back if this session put something on the stack, else to a known screen. */
export function back(fallbackPath = '/') {
  if (depth > 0 && history.length > 1) {
    depth--;
    history.back();
  } else {
    go(fallbackPath, { replace: true });
  }
}

export function hrefFor(path) {
  return `#${path}`;
}
