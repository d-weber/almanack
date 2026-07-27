// Bootstrap and application shell.
//
// Boot order matters: config first (it carries the family timezone, the app
// version and the VAPID key), then the locale, then /me. Nothing renders before
// the timezone is known — a screen drawn in the device zone would be wrong.

import { h, clear, mount } from './dom.js';
import { icon } from './icons.js';
import { t, loadLang, pickLang } from './i18n.js';
import { api, bus, setBootVersion, isStale } from './api.js';
import {
  state, applyConfig, applyMe, clearSession, applyTheme, setTheme, nextTheme,
  applyCollapsed, toggleCollapsed, setView, weekStart,
  loadRange, invalidateRange, isVisible, toggleCalendar, rememberLang, rememberedLang,
  iosDismissed, calendarImageURL,
} from './state.js';
import {
  todayISO, addDays, addMonths, startOfMonth, formatMonthTitle, formatDateShort,
} from './dates.js';
import { normalizeHex, readableOn } from './colors.js';
import { spinner, errorBox, banner } from './ui.js';
import { route, notFound, start, go, current, reload } from './router.js';
import { confirmPush, needsIOSInstall } from './push.js';

import { renderMonth, monthRange, openDaySheet } from './views/month.js';
import { renderWeek, weekRange } from './views/week.js';
import { renderAgenda } from './views/agenda.js';
import { renderEventDetail, renderEventEditor } from './views/event.js';
import { renderSearch } from './views/search.js';
import { renderActivity } from './views/activity.js';
import { renderSettings } from './views/settings.js';
import { renderCalendarList, renderCalendarDetail } from './views/calendars.js';
import { renderLogin, renderForgot, renderReset } from './views/auth.js';
import { renderJoin } from './views/join.js';
import { renderIOSInstall } from './views/iosinstall.js';

const PUBLIC_PREFIXES = ['/login', '/forgot', '/reset/', '/join/', '/ios-install'];
const REFRESH_THROTTLE_MS = 10000;
const CONFIRM_THROTTLE_MS = 60 * 60 * 1000;

// The breakpoint where the sidebar exists. Two things move at it: the calendar
// filters (sidebar above it, a row under the app bar below it) and the theme and
// collapse controls. Crossing it has to repaint, or the filters end up nowhere.
const DESKTOP = window.matchMedia('(min-width: 900px)');

const root = document.getElementById('app');
let chromeEl = null;
let viewEl = null;
let tabsEl = null;
let bannerEl = null;
let panelEl = null;
let renderToken = 0;
let panelToken = 0;
let lastRefresh = 0;
let lastConfirm = 0;
let pendingHash = null;

// ---------------------------------------------------------------------------
// Shell
// ---------------------------------------------------------------------------

function buildShell() {
  bannerEl = h('div', { class: 'banners' });
  chromeEl = h('div', { class: 'chrome' });
  viewEl = h('main', { class: 'view', id: 'view' });
  panelEl = h('aside', { class: 'panel', hidden: true });
  tabsEl = h('nav', { class: 'tabbar' });
  mount(root, bannerEl, chromeEl, viewEl, panelEl, tabsEl);
}

// On a wide screen an event opens beside the calendar rather than replacing it, so
// you can still see the week you are editing against. There is no room for that on a
// phone, where the same routes render full screen exactly as before.
function openPanel() {
  panelEl.hidden = false;
  root.classList.add('is-panel-open');
}

function closePanel() {
  if (panelEl.hidden) return;
  panelEl.hidden = true;
  clear(panelEl);
  root.classList.remove('is-panel-open');
}

function isPublicPath(path) {
  return PUBLIC_PREFIXES.some((p) => (p.endsWith('/') ? path.startsWith(p) : path === p));
}

function paintBanners() {
  clear(bannerEl);
  if (state.authed && (isStale() || !navigator.onLine)) {
    bannerEl.appendChild(banner(t('error.offline'), { kind: 'warn' }));
  }
}

function tab(name, iconName, labelKey, path) {
  const active = current().path === path || current().path.startsWith(`${path}/`)
    || (name === 'calendar' && ['/month', '/week', '/agenda', '/day'].some((p) => current().path.startsWith(p)));
  return h('button', {
    class: ['tab', active ? 'is-active' : ''].filter(Boolean).join(' '),
    type: 'button',
    'aria-current': active ? 'page' : null,
    onclick: () => go(name === 'calendar' ? `/${state.view}` : path),
  }, icon(iconName), h('span', { class: 'tab-label' }, t(labelKey)));
}

function paintTabs(visible) {
  clear(tabsEl);
  tabsEl.hidden = !visible;
  if (!visible) return;
  tabsEl.appendChild(tab('calendar', 'calendar', 'nav.calendar', '/month'));
  tabsEl.appendChild(tab('search', 'search', 'nav.search', '/search'));
  tabsEl.appendChild(tab('activity', 'activity', 'nav.activity', '/activity'));
  tabsEl.appendChild(tab('settings', 'settings', 'nav.settings', '/settings'));
  if (DESKTOP.matches && state.calendars.length) {
    tabsEl.appendChild(h('div', { class: 'side-cals' }, calendarChips()));
  }
  tabsEl.appendChild(sidebarFoot());
}

// The sidebar's footer: theme and the collapse control. It is part of the same
// element as the bottom tab bar on a phone, where CSS hides it — the controls it
// holds are desktop affordances (a phone follows its own system theme, and there is
// no sidebar to collapse).
function sidebarFoot() {
  const themeName = t(`theme.${state.theme}`);
  const themeIcon = { auto: 'themeAuto', light: 'sun', dark: 'moon' }[state.theme] || 'themeAuto';

  const themeBtn = h('button', {
    class: 'tab tab-foot',
    type: 'button',
    title: t('theme.change', { mode: themeName }),
    'aria-label': t('theme.change', { mode: themeName }),
    onclick: () => {
      setTheme(nextTheme());
      paintTabs(true);
    },
  }, icon(themeIcon), h('span', { class: 'tab-label' }, themeName));

  const collapseLabel = state.collapsed ? t('sidebar.expand') : t('sidebar.collapse');
  const collapseBtn = h('button', {
    class: 'tab tab-foot',
    type: 'button',
    title: collapseLabel,
    'aria-label': collapseLabel,
    'aria-expanded': state.collapsed ? 'false' : 'true',
    onclick: () => {
      toggleCollapsed();
      paintTabs(true);
    },
  }, icon(state.collapsed ? 'sidebarExpand' : 'sidebarCollapse'),
     h('span', { class: 'tab-label' }, collapseLabel));

  return h('div', { class: 'tabbar-foot' }, themeBtn, collapseBtn);
}

/**
 * The miniature that identifies a calendar: its uploaded picture when it has one,
 * otherwise a calendar glyph tinted with the calendar's colour. Both render at the
 * same size so a list of them lines up whether or not every calendar has a picture.
 */
export function calendarMark(cal, extraClass = '') {
  const cls = ['cal-mark', extraClass].filter(Boolean).join(' ');
  if (cal.has_image) {
    return h('img', {
      class: `${cls} cal-mark-img`,
      src: calendarImageURL(cal.id),
      alt: '',
      loading: 'lazy',
      decoding: 'async',
    });
  }
  return h('span', { class: `${cls} cal-mark-icon` }, icon('calendar'));
}

function calendarChips() {
  const row = h('div', { class: 'cal-chips scroll-x' });
  for (const cal of state.calendars) {
    const on = isVisible(cal.id);
    const hex = normalizeHex(cal.color);
    row.appendChild(h('button', {
      class: ['cal-chip', on ? 'is-on' : ''].filter(Boolean).join(' '),
      type: 'button',
      'aria-pressed': String(on),
      'aria-label': `${t('calendar.show')} ${cal.name}`,
      title: cal.name,
      style: { '--c': hex, '--c-on': readableOn(hex) },
      onclick: () => { toggleCalendar(cal.id); reload(); },
    }, calendarMark(cal), h('span', { class: 'cal-chip-name' }, cal.name)));
  }
  return row;
}

function calendarHeader(view, date) {
  const target = (dir) => (view === 'week' ? addDays(date, dir * 7) : startOfMonth(addMonths(date, dir)));
  const step = (dir) => go(`/${view}?d=${target(dir)}`);
  // No catalog string says "previous month", so the arrows announce where they
  // lead — which is more useful anyway.
  const stepLabel = (dir) => (view === 'week' ? formatDateShort(target(dir)) : formatMonthTitle(target(dir)));

  let title;
  if (view === 'week') {
    const r = weekRange(date, weekStart());
    title = `${formatDateShort(r.from)} – ${formatDateShort(r.to)}`;
  } else {
    title = formatMonthTitle(date);
  }

  const viewSwitch = h('div', { class: 'segmented view-switch' },
    ...[['month', 'view.month'], ['week', 'view.week'], ['agenda', 'view.agenda']].map(([v, key]) =>
      h('button', {
        class: ['segment', v === view ? 'is-active' : ''].filter(Boolean).join(' '),
        type: 'button',
        'aria-selected': String(v === view),
        onclick: () => go(`/${v}?d=${date}`),
      }, t(key))));

  // One line: navigation on the left, the view switch centred, and the two things
  // you start rather than navigate — search and add — on the right.
  const bar = h('header', { class: 'app-bar' },
    h('div', { class: 'app-bar-row' },
      h('div', { class: 'app-bar-nav' },
        h('button', { class: 'btn btn-quiet btn-small', type: 'button', onclick: () => go(`/${view}?d=${todayISO()}`) }, t('action.today')),
        h('button', { class: 'icon-btn', type: 'button', 'aria-label': stepLabel(-1), onclick: () => step(-1) }, icon('chevronLeft')),
        h('button', { class: 'icon-btn', type: 'button', 'aria-label': stepLabel(1), onclick: () => step(1) }, icon('chevronRight')),
        h('h1', { class: 'app-title' }, title)),
      viewSwitch,
      h('div', { class: 'app-bar-actions' },
        h('button', {
          class: 'icon-btn', type: 'button', 'aria-label': t('nav.search'), title: t('nav.search'),
          onclick: () => go('/search'),
        }, icon('search')),
        h('button', {
          class: 'icon-btn icon-btn-primary', type: 'button', 'aria-label': t('event.new'), title: t('event.new'),
          onclick: () => go(`/event/new?date=${date}`),
        }, icon('plus')))));

  if (!DESKTOP.matches) bar.appendChild(calendarChips());
  return bar;
}

/** Swap the main area, discarding results of a navigation that was superseded. */
async function show(builder, { chrome = null, tabs = true } = {}) {
  const token = ++renderToken;
  closePanel();
  paintBanners();
  clear(chromeEl);
  if (chrome) chromeEl.appendChild(chrome);
  paintTabs(tabs);
  mount(viewEl, spinner());
  viewEl.scrollTop = 0;
  try {
    const node = await builder();
    if (token !== renderToken) return;
    mount(viewEl, node);
  } catch (err) {
    if (token !== renderToken) return;
    mount(viewEl, errorBox(err, () => reload()));
  }
  paintBanners();
}

// ---------------------------------------------------------------------------
// Screens
// ---------------------------------------------------------------------------

async function calendarScreen(view, ctx) {
  setView(view);
  const date = ctx.query.get('d') || state.cursor || todayISO();
  state.cursor = date;

  const header = calendarHeader(view, date);

  await show(async () => {
    if (view === 'agenda') return renderAgenda({ date: todayISO() });
    const r = view === 'week' ? weekRange(date, weekStart()) : monthRange(date, weekStart());
    await loadRange(r.from, r.to);
    return view === 'week' ? renderWeek({ date }) : renderMonth({ date });
  }, { chrome: header, tabs: true });
}

function plainScreen(builder) {
  return show(builder, { chrome: null, tabs: false });
}

// panelScreen renders an event next to the calendar on a wide screen, and full
// screen on a narrow one. The calendar underneath is re-rendered first, so the panel
// always opens against the month or week you were actually looking at.
async function panelScreen(builder, ctx) {
  if (!DESKTOP.matches) {
    return plainScreen(builder);
  }
  // params first: /event/:id/:date carries the occurrence being opened, which is the
  // month the calendar beside it should be showing. A push notification deep-links in
  // this shape, and it used to land on today's month next to an event in October.
  const date = (ctx && ctx.params && ctx.params.date)
    || (ctx && ctx.query && ctx.query.get('date'))
    || state.cursor || todayISO();
  const view = state.view === 'agenda' ? 'agenda' : state.view;
  await calendarScreen(view, { query: new URLSearchParams(`d=${date}`) });

  const token = ++panelToken;
  openPanel();
  mount(panelEl, spinner());
  try {
    const node = await builder();
    if (token !== panelToken) return;
    mount(panelEl, node);
    panelEl.scrollTop = 0;
  } catch (err) {
    if (token !== panelToken) return;
    mount(panelEl, errorBox(err, () => reload()));
  }
}

function tabScreen(builder) {
  return show(builder, { chrome: null, tabs: true });
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

function guardAuth(ctx) {
  if (state.authed) return true;
  if (isPublicPath(ctx.path)) return true;
  pendingHash = location.hash && location.hash !== '#/login' ? location.hash : null;
  go('/login', { replace: true });
  return false;
}

function registerRoutes() {
  route('/', () => go(`/${state.view}`, { replace: true }));

  for (const view of ['month', 'week', 'agenda']) {
    route(`/${view}`, (ctx) => { if (guardAuth(ctx)) calendarScreen(view, ctx); });
  }

  route('/day/:date', async (ctx) => {
    if (!guardAuth(ctx)) return;
    await calendarScreen('month', { query: new URLSearchParams(`d=${ctx.params.date}`) });
    openDaySheet(ctx.params.date);
  });

  route('/event/new', (ctx) => {
    if (!guardAuth(ctx)) return;
    panelScreen(() => renderEventEditor({ query: ctx.query }), ctx);
  });
  route('/event/:id/:date', (ctx) => {
    if (!guardAuth(ctx)) return;
    panelScreen(() => renderEventDetail(ctx.params), ctx);
  });
  route('/event/:id/:date/edit', (ctx) => {
    if (!guardAuth(ctx)) return;
    panelScreen(() => renderEventEditor({ id: ctx.params.id, date: ctx.params.date, query: ctx.query }), ctx);
  });

  route('/search', (ctx) => { if (guardAuth(ctx)) tabScreen(() => renderSearch()); });
  route('/activity', (ctx) => { if (guardAuth(ctx)) tabScreen(() => renderActivity()); });
  route('/settings', (ctx) => { if (guardAuth(ctx)) tabScreen(() => renderSettings()); });
  route('/calendars', (ctx) => { if (guardAuth(ctx)) plainScreen(() => renderCalendarList()); });
  route('/calendars/:id', (ctx) => { if (guardAuth(ctx)) plainScreen(() => renderCalendarDetail(ctx.params)); });

  route('/login', () => plainScreen(() => renderLogin()));
  route('/forgot', () => plainScreen(() => renderForgot()));
  route('/reset/:token', (ctx) => plainScreen(() => renderReset(ctx.params)));
  route('/join/:token', (ctx) => plainScreen(() => renderJoin(ctx.params)));
  route('/ios-install', () => plainScreen(() => renderIOSInstall({ dismissible: true })));

  notFound(() => go('/', { replace: true }));
}

// ---------------------------------------------------------------------------
// Freshness
// ---------------------------------------------------------------------------

const REFRESHABLE = ['/month', '/week', '/agenda', '/day'];

/**
 * Refetch the visible range on focus. Only the calendar screens are re-rendered:
 * rebuilding the editor under a half-typed event would lose it, so elsewhere the
 * cache is merely invalidated and the next visit reloads.
 */
function refreshNow(force = false) {
  if (!state.authed) return;
  const now = Date.now();
  if (!force && now - lastRefresh < REFRESH_THROTTLE_MS) return;
  lastRefresh = now;
  invalidateRange();
  const path = current().path;
  if (REFRESHABLE.some((p) => path === p || path.startsWith(`${p}/`))) reload();
}

function confirmPushIfDue() {
  if (!state.authed) return;
  const now = Date.now();
  if (now - lastConfirm < CONFIRM_THROTTLE_MS) return;
  lastConfirm = now;
  const key = state.config && state.config.vapid_public_key;
  confirmPush(key).catch(() => { /* offline; retried on the next open */ });
}

function wireEvents() {
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState !== 'visible') return;
    refreshNow();
    confirmPushIfDue();
  });
  window.addEventListener('focus', () => refreshNow());
  window.addEventListener('online', () => { paintBanners(); refreshNow(true); });
  window.addEventListener('offline', () => paintBanners());

  // Rotating a tablet crosses the sidebar breakpoint, and the calendar filters have
  // to follow it from the sidebar to the app bar or they would simply vanish.
  DESKTOP.addEventListener('change', () => { if (state.authed) reload(); });

  bus.addEventListener('unauthorized', () => {
    clearSession();
    pendingHash = location.hash && !location.hash.startsWith('#/login') ? location.hash : null;
    go('/login', { replace: true });
  });

  bus.addEventListener('offline', () => paintBanners());

  bus.addEventListener('authenticated', async () => {
    try {
      await loadSession();
    } catch (err) {
      clearSession();
      go('/login', { replace: true });
      return;
    }
    const target = pendingHash;
    pendingHash = null;
    go(target ? target.replace(/^#/, '') : '/', { replace: true });
    afterLogin();
  });
}

function registerServiceWorker() {
  if (!('serviceWorker' in navigator)) return;
  navigator.serviceWorker.register('/sw.js').catch((err) => console.warn('sw', err));
  let reloading = false;
  navigator.serviceWorker.addEventListener('message', (event) => {
    const data = event.data;
    if (!data) return;
    if (data.type === 'reload' && !reloading) {
      reloading = true;
      location.reload();
      return;
    }
    // A notification click on an already-open window routes in place.
    if (data.type === 'navigate' && typeof data.url === 'string') {
      const hash = data.url.indexOf('#') === -1 ? '/' : data.url.slice(data.url.indexOf('#') + 1);
      go(hash || '/');
    }
  });
}

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

async function loadSession() {
  const me = await api.me();
  applyMe(me);
  if (me.user && me.user.lang) {
    rememberLang(me.user.lang);
    await loadLang(pickLang(me.user.lang));
  }
  return me;
}

function afterLogin() {
  confirmPushIfDue();
  if (needsIOSInstall() && !iosDismissed()) go('/ios-install');
}

async function boot() {
  // Invite and password-reset links were once emitted without the "#/", and some are
  // already in inboxes. Translate them rather than dropping their holder on the login
  // screen with no way forward.
  const deepLink = location.pathname.match(/^\/(join|reset)\/([^/]+)$/);
  if (deepLink && !location.hash) {
    history.replaceState(null, '', `/#/${deepLink[1]}/${deepLink[2]}`);
  }

  buildShell();
  applyTheme();
  applyCollapsed();
  mount(viewEl, spinner());

  let cfg = null;
  try {
    cfg = await api.config();
    applyConfig(cfg);
    setBootVersion(cfg.app_version);
  } catch (_) { /* offline first-run: fall back to defaults and the cached shell */ }

  await loadLang(pickLang(rememberedLang())).catch(() => { /* keys render as keys */ });

  try {
    await loadSession();
  } catch (_) {
    clearSession();
  }

  registerRoutes();
  wireEvents();
  start();

  if (state.authed) {
    confirmPushIfDue();
    if (needsIOSInstall() && !iosDismissed() && !isPublicPath(current().path)) go('/ios-install');
  }

  registerServiceWorker();
}

boot();
