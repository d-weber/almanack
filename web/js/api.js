// Fetch wrapper for /api/v1 (docs/api-contract.md is normative).
//
// Responsibilities beyond fetch():
//   - X-Requested-With: almanack on every mutation (the CSRF defense; there is no token);
//   - same-origin credentials, so the session cookie rides along;
//   - {"error":{"code":…}} -> ApiError, which views map to error.<code> catalog keys;
//   - X-App-Version handshake: one hard reload when the server has moved on;
//   - X-From-Cache (set by the service worker) -> offline banner;
//   - 401 outside the auth routes -> the app routes to the login screen.

export class ApiError extends Error {
  constructor(code, status, message) {
    super(message || code);
    this.name = 'ApiError';
    this.code = code;
    this.status = status;
  }
}

/** Locale key for any error, so views never branch on codes themselves. */
export function errorKey(err) {
  const code = err instanceof ApiError ? err.code : 'internal';
  const known = ['unauthorized', 'forbidden', 'not_found', 'invalid', 'conflict',
    'rate_limited', 'internal', 'network', 'offline'];
  return `error.${known.includes(code) ? code : 'internal'}`;
}

export const bus = new EventTarget();

function emit(type, detail) {
  bus.dispatchEvent(new CustomEvent(type, { detail }));
}

const BASE = '/api/v1';
let bootVersion = null;
let servedFromCache = false;

export function setBootVersion(v) {
  if (v) bootVersion = v;
}

export function bootedVersion() {
  return bootVersion;
}

/** True when the last successful read came out of the service-worker cache. */
export function isStale() {
  return servedFromCache;
}

function checkVersion(res) {
  const v = res.headers.get('X-App-Version');
  if (!v) return;
  if (!bootVersion) {
    bootVersion = v;
    return;
  }
  if (v === bootVersion) return;
  // One reload, ever, per version: a mismatch that survives it is a server bug and
  // must not become a refresh loop.
  const guard = `almanack.reloaded.${v}`;
  try {
    if (sessionStorage.getItem(guard)) return;
    sessionStorage.setItem(guard, '1');
  } catch (_) { /* private mode: accept the small risk of a second reload */ }
  emit('version', { version: v });

  // Ask the service worker for the new build BEFORE reloading. A bare reload is
  // served from the existing precache, so it comes back running exactly the same
  // stale code — and having spent the one-reload-per-version guard, the device stays
  // pinned to the old build indefinitely, talking to a newer server with no signal
  // that anything is wrong. When the update lands, the worker's own activate step
  // messages open clients and the reload happens there; this timeout is only the
  // fallback for a browser with no service worker at all.
  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.getRegistration()
      .then((reg) => (reg ? reg.update() : null))
      .catch(() => {})
      .finally(() => setTimeout(() => location.reload(), 1500));
    return;
  }
  location.reload();
}

async function parseError(res) {
  let code = null;
  let message = '';
  try {
    const body = await res.json();
    if (body && body.error) {
      code = body.error.code;
      message = body.error.message || '';
    }
  } catch (_) { /* non-JSON error body (proxy page, 502…) */ }
  if (!code) {
    if (res.status === 401) code = 'unauthorized';
    else if (res.status === 403) code = 'forbidden';
    else if (res.status === 404) code = 'not_found';
    else if (res.status === 409) code = 'conflict';
    else if (res.status === 429) code = 'rate_limited';
    else if (res.status >= 400 && res.status < 500) code = 'invalid';
    else code = 'internal';
  }
  return new ApiError(code, res.status, message);
}

async function request(method, path, { body, raw, contentType, signal } = {}) {
  const headers = { Accept: 'application/json' };
  const mutation = method !== 'GET' && method !== 'HEAD';
  if (mutation) headers['X-Requested-With'] = 'almanack';

  let payload;
  if (raw !== undefined) {
    payload = raw;
    if (contentType) headers['Content-Type'] = contentType;
  } else if (body !== undefined) {
    payload = JSON.stringify(body);
    headers['Content-Type'] = 'application/json';
  }

  let res;
  try {
    res = await fetch(BASE + path, {
      method,
      headers,
      body: payload,
      credentials: 'same-origin',
      signal,
    });
  } catch (err) {
    if (err && err.name === 'AbortError') throw err;
    servedFromCache = true;
    emit('offline', { offline: true });
    throw new ApiError('network', 0, String(err && err.message));
  }

  checkVersion(res);

  const cached = res.headers.get('X-From-Cache') === '1';
  if (cached !== servedFromCache) {
    servedFromCache = cached;
    emit('offline', { offline: cached });
  }

  if (!res.ok) {
    const err = await parseError(res);
    if (err.status === 401 && !path.startsWith('/auth/')) emit('unauthorized', {});
    throw err;
  }
  if (res.status === 204) return null;
  const type = res.headers.get('Content-Type') || '';
  if (!type.includes('json')) return null;
  return res.json();
}

function qs(params) {
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(params || {})) {
    if (v === undefined || v === null || v === '') continue;
    sp.set(k, String(v));
  }
  const s = sp.toString();
  return s ? `?${s}` : '';
}

export const api = {
  // -- public ---------------------------------------------------------------
  config: () => request('GET', '/config'),
  invitePreview: (token) => request('GET', `/invites/${encodeURIComponent(token)}`),
  signup: (body) => request('POST', '/auth/signup', { body }),
  login: (email, password) => request('POST', '/auth/login', { body: { email, password } }),
  resetRequest: (email) => request('POST', '/auth/password-reset/request', { body: { email } }),
  resetConfirm: (token, password) => request('POST', '/auth/password-reset/confirm', { body: { token, password } }),

  // -- session --------------------------------------------------------------
  me: () => request('GET', '/me'),
  patchMe: (body) => request('PATCH', '/me', { body }),
  putAvatar: (blob) => request('PUT', '/me/avatar', { raw: blob, contentType: blob.type || 'image/jpeg' }),
  deleteAvatar: () => request('DELETE', '/me/avatar'),
  putCalendarImage: (id, blob) => request('PUT', `/calendars/${id}/image`, { raw: blob, contentType: blob.type || 'image/jpeg' }),
  deleteCalendarImage: (id) => request('DELETE', `/calendars/${id}/image`),
  // Bodyless POSTs still send `{}` so the server always sees the declared
  // Content-Type: application/json.
  logout: () => request('POST', '/auth/logout', { body: {} }),

  // -- calendars ------------------------------------------------------------
  createCalendar: (body) => request('POST', '/calendars', { body }),
  patchCalendar: (id, body) => request('PATCH', `/calendars/${id}`, { body }),
  deleteCalendar: (id) => request('DELETE', `/calendars/${id}`),
  leaveCalendar: (id) => request('POST', `/calendars/${id}/leave`, { body: {} }),
  patchMembership: (id, body) => request('PATCH', `/calendars/${id}/membership`, { body }),
  removeMember: (id, userId) => request('DELETE', `/calendars/${id}/members/${userId}`),
  patchLabel: (id, labelId, body) => request('PATCH', `/calendars/${id}/labels/${labelId}`, { body }),
  createInvite: (id) => request('POST', `/calendars/${id}/invites`, { body: {} }),
  listInvites: (id) => request('GET', `/calendars/${id}/invites`),
  revokeInvite: (inviteId) => request('POST', `/invites/${inviteId}/revoke`, { body: {} }),

  // -- events ---------------------------------------------------------------
  events: (from, to, calendarIds, signal) =>
    request('GET', `/events${qs({ from, to, calendar_ids: calendarIds && calendarIds.length ? calendarIds.join(',') : undefined })}`, { signal }),
  event: (id, date) => request('GET', `/events/${id}${qs({ date })}`),
  createEvent: (body) => request('POST', '/events', { body }),
  updateEvent: (id, body, { scope, date } = {}) =>
    request('PATCH', `/events/${id}${qs({ scope, date })}`, { body }),
  deleteEvent: (id, { scope, date } = {}) =>
    request('DELETE', `/events/${id}${qs({ scope, date })}`),
  putReminders: (id, reminders) => request('PUT', `/events/${id}/reminders`, { body: { reminders } }),
  search: (params) => request('GET', `/search${qs(params)}`),

  // -- notifications --------------------------------------------------------
  prefs: () => request('GET', '/prefs'),
  patchPrefs: (body) => request('PATCH', '/prefs', { body }),
  pushSubscribe: (body) => request('POST', '/push/subscription', { body }),
  pushConfirm: (endpoint) => request('POST', '/push/confirm', { body: { endpoint } }),
  pushUnsubscribe: (endpoint) => request('DELETE', '/push/subscription', { body: { endpoint } }),
  pushTest: () => request('POST', '/push/test', { body: {} }),
  activity: (params) => request('GET', `/activity${qs(params)}`),
};
