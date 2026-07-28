// In-memory store. One object, a subscriber list, and a small localStorage-backed
// set of per-device preferences (which calendars are hidden, colour mode, theme,
// last view). Server-side preferences never live here — they live on /me.

import { api } from './api.js';
import { setTimezone, setTimeFormat, todayISO, compareOccurrences, occDays } from './dates.js';

const LS = {
  hidden: 'almanack.hiddenCalendars',
  colorBy: 'almanack.colorBy',
  theme: 'almanack.theme',
  collapsed: 'almanack.sidebarCollapsed',
  view: 'almanack.view',
  lang: 'almanack.lang',
  iosDismissed: 'almanack.iosInstallDismissed',
};

function lsGet(key, fallback) {
  try {
    const v = localStorage.getItem(key);
    return v === null ? fallback : v;
  } catch (_) {
    return fallback;
  }
}

function lsSet(key, value) {
  try {
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, value);
  } catch (_) { /* private mode: preferences simply do not persist */ }
}

export const state = {
  config: null,
  user: null,
  prefs: null,
  calendars: [],
  familyTz: 'Europe/Paris',
  appVersion: '',
  authed: false,
  offline: false,
  view: ['month', 'week', 'agenda'].includes(lsGet(LS.view, 'month')) ? lsGet(LS.view, 'month') : 'month',
  cursor: null,            // the date the calendar screens are centred on
  colorBy: lsGet(LS.colorBy, 'label') === 'person' ? 'person' : 'label',
  theme: lsGet(LS.theme, 'auto'),
  // Collapsing the sidebar is a per-device choice, like which calendars are hidden:
  // the same person wants it open on a laptop and out of the way on a small screen.
  collapsed: lsGet(LS.collapsed, '0') === '1',
  hidden: new Set(),
  range: { from: null, to: null, occurrences: [], byDate: new Map(), holidays: new Map() },
};

try {
  const raw = JSON.parse(lsGet(LS.hidden, '[]'));
  if (Array.isArray(raw)) state.hidden = new Set(raw.map(Number));
} catch (_) { /* corrupt value: start with everything visible */ }

// ---------------------------------------------------------------------------
// Subscriptions
// ---------------------------------------------------------------------------

const listeners = new Set();

export function subscribe(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export function notify(reason) {
  for (const fn of Array.from(listeners)) {
    try {
      fn(reason);
    } catch (err) {
      console.error('state listener', err);
    }
  }
}

// ---------------------------------------------------------------------------
// Session data
// ---------------------------------------------------------------------------

export function applyConfig(cfg) {
  state.config = cfg || null;
  if (cfg) {
    setTimezone(cfg.family_tz);
    state.familyTz = cfg.family_tz || state.familyTz;
    state.appVersion = cfg.app_version || state.appVersion;
    applyHolidayColor(cfg.holiday_color);
  }
}

/**
 * Public holidays belong to no calendar, so their colour is an operator setting
 * rather than a label. It reaches the stylesheet as a token; the fallback in
 * style.css is what a browser paints in the moment before /config answers.
 */
function applyHolidayColor(color) {
  if (!/^#[0-9a-fA-F]{6}$/.test(String(color || ''))) return;
  document.documentElement.style.setProperty('--holiday', color);
}

/**
 * Apply a /me. Everything that can fail is computed before anything is written.
 *
 * /me carries the family timezone, exactly as /config does, and the first date this
 * function converts through it is the cursor. A browser that cannot resolve the zone
 * throws there — and this used to set `authed` and half the session first, so what came
 * out was an application that believed it was signed in with no date to draw on. The
 * throw then travelled out of loadSession() into a catch that reads a failure there as
 * "not signed in", and the right password gave you the login form again, and again
 * (#58). Nothing half-applies now, so a session that cannot be applied is simply the
 * one that was there before; app.js is where the browser's answer to the zone is acted
 * on, at every point one can arrive.
 */
export function applyMe(me) {
  const tz = me.family_tz || state.familyTz;
  setTimezone(tz);
  const cursor = state.cursor || todayISO();

  state.user = me.user;
  state.prefs = me.prefs;
  state.calendars = Array.isArray(me.calendars) ? me.calendars : [];
  state.familyTz = tz;
  state.appVersion = me.app_version || state.appVersion;
  state.authed = true;
  setTimeFormat(me.user && me.user.time_format);
  state.cursor = cursor;
  // Drop stale hidden ids so a deleted calendar cannot hide a new one by id reuse.
  const known = new Set(state.calendars.map((c) => c.id));
  for (const id of Array.from(state.hidden)) if (!known.has(id)) state.hidden.delete(id);
}

/** Re-read /me after any change to calendars, labels, members or the profile. */
export async function refreshMe() {
  const me = await api.me();
  applyMe(me);
  notify('me');
  return me;
}

export function clearSession() {
  state.user = null;
  state.prefs = null;
  state.calendars = [];
  state.authed = false;
  state.range = { from: null, to: null, occurrences: [], byDate: new Map(), holidays: new Map() };
  purgeApiCache();
}

/**
 * Clearing the session in memory is only half of it: the service worker keeps the
 * last-seen /api/ responses so the calendar reads offline, and those are the family's
 * appointments sitting on the device after somebody has signed out of it. Worse, the
 * cached /me answers 200, so an offline boot would take the app straight back into a
 * calendar nobody is signed in to. Every path that ends a session — the sign-out
 * button, a 401 from the server, a boot that could not load one — comes through
 * clearSession(), which is why the request belongs here and nowhere else.
 */
function purgeApiCache() {
  try {
    const sw = navigator.serviceWorker;
    if (!sw) return; // no worker on this device, so nothing of ours was cached
    if (sw.controller) {
      sw.controller.postMessage({ type: 'purgeApi' });
      return;
    }
    // An uncontrolled page has no controller but the cache is still there: a hard
    // reload loads outside the worker's control, and so does the first load after it
    // installs. Signing out from one of those left every /api/ response on the device
    // — the family's appointments, and a /me that answers 200, which takes an offline
    // boot straight back into a calendar nobody is signed in to. The registration's
    // active worker can be messaged either way, so that is what is asked for. A device
    // with no registration at all never settles, which is the right answer: there is
    // nothing of ours cached to purge.
    sw.ready.then((reg) => {
      if (reg && reg.active) reg.active.postMessage({ type: 'purgeApi' });
    }).catch(() => {});
  } catch (_) { /* nothing cached, or a browser that forbids asking */ }
}

export function weekStart() {
  const w = state.user && Number(state.user.week_start);
  return Number.isInteger(w) && w >= 0 && w <= 6 ? w : 1;
}

export function rememberLang(lang) {
  lsSet(LS.lang, lang || null);
}

export function rememberedLang() {
  return lsGet(LS.lang, null);
}

// ---------------------------------------------------------------------------
// Lookups
// ---------------------------------------------------------------------------

export function calendarById(id) {
  return state.calendars.find((c) => c.id === Number(id)) || null;
}

export function labelsOf(calendarId) {
  const cal = calendarById(calendarId);
  if (!cal || !Array.isArray(cal.labels)) return [];
  return cal.labels.slice().sort((a, b) => a.position - b.position);
}

export function labelById(calendarId, labelId) {
  return labelsOf(calendarId).find((l) => l.id === Number(labelId)) || null;
}

export function membersOf(calendarId) {
  const cal = calendarById(calendarId);
  return cal && Array.isArray(cal.members) ? cal.members : [];
}

/** Every distinct person across the caller's calendars. */
export function allMembers() {
  const seen = new Map();
  for (const cal of state.calendars) {
    for (const m of cal.members || []) if (!seen.has(m.user_id)) seen.set(m.user_id, m);
  }
  return Array.from(seen.values()).sort((a, b) =>
    String(a.display_name).localeCompare(String(b.display_name)));
}

export function memberById(userId) {
  const id = Number(userId);
  for (const cal of state.calendars) {
    for (const m of cal.members || []) if (m.user_id === id) return m;
  }
  return null;
}

/** Participant member records for an occurrence, in calendar member order. */
export function participantsOf(occ) {
  const ids = Array.isArray(occ.participants) ? occ.participants : [];
  const members = membersOf(occ.calendar_id);
  const out = [];
  for (const m of members) if (ids.includes(m.user_id)) out.push(m);
  for (const id of ids) {
    if (out.some((m) => m.user_id === id)) continue;
    const m = memberById(id);
    if (m) out.push(m);
  }
  return out;
}

export function avatarURL(userId) {
  return `/api/v1/users/${Number(userId)}/avatar`;
}

export function calendarImageURL(calendarId) {
  return `/api/v1/calendars/${Number(calendarId)}/image`;
}

// ---------------------------------------------------------------------------
// Per-device view preferences
// ---------------------------------------------------------------------------

export function isVisible(calendarId) {
  return !state.hidden.has(Number(calendarId));
}

export function toggleCalendar(calendarId) {
  const id = Number(calendarId);
  if (state.hidden.has(id)) state.hidden.delete(id);
  else state.hidden.add(id);
  lsSet(LS.hidden, JSON.stringify(Array.from(state.hidden)));
  reindexRange();
  notify('calendars');
}

export function setColorBy(mode) {
  state.colorBy = mode === 'person' ? 'person' : 'label';
  lsSet(LS.colorBy, state.colorBy);
  notify('colorBy');
}

export function setTheme(theme) {
  state.theme = ['auto', 'light', 'dark'].includes(theme) ? theme : 'auto';
  lsSet(LS.theme, state.theme);
  applyTheme();
  notify('theme');
}

export function applyTheme() {
  const root = document.documentElement;
  if (state.theme === 'auto') root.removeAttribute('data-theme');
  else root.setAttribute('data-theme', state.theme);
}

/** THEMES is the cycle order of the theme button: system, then the two overrides. */
export const THEMES = ['auto', 'light', 'dark'];

export function nextTheme() {
  return THEMES[(THEMES.indexOf(state.theme) + 1) % THEMES.length];
}

export function setCollapsed(collapsed) {
  state.collapsed = Boolean(collapsed);
  lsSet(LS.collapsed, state.collapsed ? '1' : '0');
  applyCollapsed();
  notify('collapsed');
}

export function toggleCollapsed() {
  setCollapsed(!state.collapsed);
}

// The class goes on the shell rather than <html> because collapsing only means
// anything where the sidebar exists; on a phone the same element is the bottom bar.
export function applyCollapsed() {
  const shell = document.getElementById('app');
  if (shell) shell.classList.toggle('is-collapsed', state.collapsed);
}

export function setView(view) {
  state.view = ['month', 'week', 'agenda'].includes(view) ? view : 'month';
  lsSet(LS.view, state.view);
}

export function iosDismissed() {
  return lsGet(LS.iosDismissed, '') === '1';
}

export function dismissIOSInstall() {
  lsSet(LS.iosDismissed, '1');
}

// ---------------------------------------------------------------------------
// Event range loading
// ---------------------------------------------------------------------------

let inflight = null;

// Which range load is the current one. Every start takes the next number and only the
// holder of the highest may write to state.range; see loadRange.
let rangeSeq = 0;

/**
 * The contract names an occurrence's event `event_id`; the Go type embeds Event,
 * whose own field is `id`. Accept either so one serialization detail cannot blank
 * out the whole calendar.
 */
export function normalizeOccurrence(occ) {
  if (occ && occ.event_id == null && occ.id != null) occ.event_id = occ.id;
  return occ;
}

/** Raw read used by screens that keep their own list (agenda, search preview). */
export async function fetchRange(from, to) {
  const data = await api.events(from, to);
  return {
    occurrences: (Array.isArray(data.occurrences) ? data.occurrences : []).map(normalizeOccurrence),
    holidays: Array.isArray(data.holidays) ? data.holidays : [],
  };
}

/**
 * Load the visible window into the store; identical windows are deduped.
 *
 * A load that has been overtaken does not write. Paging quickly through months starts a
 * request per month against one shared store, they come back in whatever order the
 * network gives them, and the slow one used to land last and win: the grid drawn from
 * August, `state.range` holding July, and the day sheet — which reads the store rather
 * than the grid — opening July's appointments on an August date. The sequence number is
 * compared after the await for that reason, and the caller is handed whatever is loaded
 * now, which is the window it was overtaken by.
 */
export async function loadRange(from, to, { force = false } = {}) {
  if (!force && state.range.loadedAt && state.range.from === from && state.range.to === to) {
    return state.range;
  }
  if (inflight && inflight.key === `${from}:${to}` && !force) return inflight.promise;

  const seq = ++rangeSeq;
  const promise = (async () => {
    const data = await fetchRange(from, to);
    if (seq !== rangeSeq) return state.range;
    state.range.from = from;
    state.range.to = to;
    state.range.occurrences = data.occurrences;
    state.range.holidays = new Map(data.holidays.map((hd) => [hd.date, hd.name]));
    state.range.loadedAt = Date.now();
    reindexRange();
    return state.range;
  })();

  inflight = { key: `${from}:${to}`, promise };
  try {
    return await promise;
  } finally {
    if (inflight && inflight.promise === promise) inflight = null;
  }
}

/** Rebuild the date index; called on load and whenever visibility changes. */
export function reindexRange() {
  const byDate = new Map();
  for (const occ of state.range.occurrences) {
    if (!isVisible(occ.calendar_id)) continue;
    for (const day of occDays(occ)) {
      let list = byDate.get(day);
      if (!list) byDate.set(day, (list = []));
      list.push(occ);
    }
  }
  for (const list of byDate.values()) list.sort(compareOccurrences);
  state.range.byDate = byDate;
}

export function occurrencesOn(dateISO) {
  return state.range.byDate.get(dateISO) || [];
}

export function holidayOn(dateISO) {
  return state.range.holidays.get(dateISO) || null;
}

/** Invalidate after any mutation, so the next render refetches. */
export function invalidateRange() {
  state.range.loadedAt = 0;
}
