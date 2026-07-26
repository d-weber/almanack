// Translation catalog. Flat dictionaries shared with the server
// (internal/i18n/locales), served at /locales/<lang>.json.
//
// No user-visible string is hardcoded in JavaScript. If a key is missing the key
// itself is rendered, which is loud enough to notice in review.

const SUPPORTED = ['fr', 'en'];
const FALLBACK = 'fr';

let current = FALLBACK;
let dict = Object.create(null);

export function currentLang() {
  return current;
}

// pickLang resolves the language to use: an explicit choice (user profile or a
// remembered one) wins, then the browser's preference, then French.
export function pickLang(preferred) {
  if (preferred && SUPPORTED.includes(preferred)) return preferred;
  const nav = Array.isArray(navigator.languages) && navigator.languages.length
    ? navigator.languages
    : [navigator.language || ''];
  for (const tag of nav) {
    const base = String(tag).toLowerCase().split('-')[0];
    if (SUPPORTED.includes(base)) return base;
  }
  return FALLBACK;
}

export async function loadLang(lang) {
  const want = SUPPORTED.includes(lang) ? lang : FALLBACK;
  const res = await fetch(`/locales/${want}.json`, { credentials: 'same-origin' });
  if (!res.ok) throw new Error(`locale ${want}: ${res.status}`);
  dict = await res.json();
  current = want;
  document.documentElement.setAttribute('lang', want);
  return want;
}

/** t('auth.signup.title', {calendar: 'Famille'}) */
export function t(key, params) {
  const raw = dict[key];
  const s = typeof raw === 'string' ? raw : key;
  if (!params) return s;
  return s.replace(/\{(\w+)\}/g, (whole, name) =>
    Object.prototype.hasOwnProperty.call(params, name) ? String(params[name]) : whole);
}

/** has() lets a view degrade gracefully instead of printing a raw key. */
export function has(key) {
  return typeof dict[key] === 'string';
}

/** weekdayName(0..6, 'long'|'short'|'narrow') — 0 is Sunday, as in JS. */
export function weekdayName(dow, form = 'long') {
  const suffix = form === 'long' ? '' : `${form}.`;
  return t(`date.weekday.${suffix}${dow}`);
}

/** monthName(1..12, short?) */
export function monthName(month, short = false) {
  return t(short ? `date.month.short.${month}` : `date.month.${month}`);
}

/** Capitalize the first letter — French month/weekday names are stored lowercase. */
export function capitalize(s) {
  return s ? s.charAt(0).toUpperCase() + s.slice(1) : s;
}
