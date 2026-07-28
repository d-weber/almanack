// Every date operation in the app goes through this module.
//
// The rule (CONVENTIONS §4, technical-plan §6): the family timezone comes from the
// server and is the ONLY timezone the UI ever uses. The device zone is never read —
// a phone in Lisbon must still show the Paris dentist at 16:30. Every
// Intl.DateTimeFormat below therefore passes `timeZone`.
//
// Two kinds of value live here and must not be confused:
//   - a *date* is a plain 'YYYY-MM-DD' string with no timezone (all-day events);
//   - an *instant* is an RFC 3339 UTC string (timed events).
// Date arithmetic uses Date.UTC() purely as an integer calendar, never as a moment,
// which is what keeps it DST-proof.

import { t, weekdayName, monthName, currentLang, capitalize } from './i18n.js';

let familyTz = 'Europe/Paris';
let timeFormat = '24h';
let unknownTz = null;

const partFormatters = new Map();

/**
 * The configured zone exists for the server and not for this browser.
 *
 * The two sides read different copies of the tz database — the server's
 * time.LoadLocation reads the operating system's, Intl reads whatever the browser
 * shipped — and a zone young enough to be in one and not the other is accepted at
 * startup and unusable here. America/Coyhaique, added in tzdata 2025a, is the case
 * that was reported. Intl says only "Invalid time zone specified", from whichever
 * date call happened to run first; this says which setting is wrong.
 */
export class UnknownTimezoneError extends Error {
  constructor(tz) {
    super(`timezone unknown to this browser: ${tz}`);
    this.name = 'UnknownTimezoneError';
    this.timezone = tz;
  }
}

export function setTimezone(tz) {
  if (typeof tz !== 'string' || !tz) return;
  familyTz = tz;
  partFormatters.clear();
  unknownTz = null;
  // Asked here rather than left for the first date call, so that boot can decide what
  // to do about it while there is still a whole screen to give the answer.
  try {
    partFormatter();
  } catch (err) {
    if (!(err instanceof UnknownTimezoneError)) throw err;
    unknownTz = tz;
  }
}

export function timezone() {
  return familyTz;
}

/**
 * The configured zone, when this browser cannot resolve it; null otherwise.
 *
 * The zone is kept rather than swapped for one that works. Every hour this app puts
 * on a screen is a conversion through it, so a substitute would not degrade the
 * calendar — it would rewrite it, plausibly, with nothing on screen to say so.
 */
export function unknownTimezone() {
  return unknownTz;
}

export function setTimeFormat(fmt) {
  timeFormat = fmt === '12h' ? '12h' : '24h';
}

export function is12h() {
  return timeFormat === '12h';
}

function partFormatter() {
  let f = partFormatters.get(familyTz);
  if (!f) {
    try {
      f = new Intl.DateTimeFormat('en-US', {
        timeZone: familyTz,
        year: 'numeric', month: '2-digit', day: '2-digit',
        hour: '2-digit', minute: '2-digit', second: '2-digit',
        hourCycle: 'h23',
      });
    } catch (err) {
      // The only thing here that can be out of range is the zone name — every other
      // option is a literal. Anything else is a different fault and travels as itself
      // rather than being renamed into this one.
      if (err instanceof RangeError) throw new UnknownTimezoneError(familyTz);
      throw err;
    }
    partFormatters.set(familyTz, f);
  }
  return f;
}

/** Wall-clock fields of an instant, in the family timezone. */
function wallParts(date) {
  const out = {};
  for (const p of partFormatter().formatToParts(date)) {
    if (p.type !== 'literal') out[p.type] = Number(p.value);
  }
  // hourCycle h23 yields 24 for midnight in some engines; normalize.
  if (out.hour === 24) out.hour = 0;
  return out;
}

/** Offset of the family timezone at a given instant, in milliseconds. */
function offsetMs(date) {
  const p = wallParts(date);
  return Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute, p.second) - date.getTime();
}

export function pad2(n) {
  return String(n).padStart(2, '0');
}

/** 'YYYY-MM-DD' from calendar fields (month is 1-based). */
export function fromParts(y, m, d) {
  return `${String(y).padStart(4, '0')}-${pad2(m)}-${pad2(d)}`;
}

/** {y, m, d} from 'YYYY-MM-DD'. */
export function parseDate(iso) {
  const s = String(iso);
  return { y: Number(s.slice(0, 4)), m: Number(s.slice(5, 7)), d: Number(s.slice(8, 10)) };
}

export function isDate(iso) {
  return typeof iso === 'string' && /^\d{4}-\d{2}-\d{2}$/.test(iso);
}

/** Integer calendar handle: a UTC midnight used only for arithmetic. */
function toUTC(iso) {
  const { y, m, d } = parseDate(iso);
  return Date.UTC(y, m - 1, d);
}

function fromUTC(ms) {
  const dt = new Date(ms);
  return fromParts(dt.getUTCFullYear(), dt.getUTCMonth() + 1, dt.getUTCDate());
}

export function addDays(iso, n) {
  return fromUTC(toUTC(iso) + n * 86400000);
}

/** Whole days between two dates (b - a). */
export function diffDays(a, b) {
  return Math.round((toUTC(b) - toUTC(a)) / 86400000);
}

/** Day of week, 0 = Sunday .. 6 = Saturday. Safe because the handle is UTC midnight. */
export function weekdayOf(iso) {
  return new Date(toUTC(iso)).getUTCDay();
}

export function daysInMonth(y, m) {
  return new Date(Date.UTC(y, m, 0)).getUTCDate();
}

/** Month arithmetic clamps the day: 31 Jan + 1 month = 28/29 Feb. */
export function addMonths(iso, n) {
  const { y, m, d } = parseDate(iso);
  const total = y * 12 + (m - 1) + n;
  const ny = Math.floor(total / 12);
  const nm = (total % 12 + 12) % 12 + 1;
  return fromParts(ny, nm, Math.min(d, daysInMonth(ny, nm)));
}

export function startOfMonth(iso) {
  const { y, m } = parseDate(iso);
  return fromParts(y, m, 1);
}

export function endOfMonth(iso) {
  const { y, m } = parseDate(iso);
  return fromParts(y, m, daysInMonth(y, m));
}

/** weekStart: 0 = Sunday .. 6 = Saturday (the display-only user preference). */
export function startOfWeek(iso, weekStart = 1) {
  const shift = (weekdayOf(iso) - weekStart + 7) % 7;
  return addDays(iso, -shift);
}

export function endOfWeek(iso, weekStart = 1) {
  return addDays(startOfWeek(iso, weekStart), 6);
}

/** The 6×7 grid of dates covering a month, aligned on weekStart. */
export function monthGrid(iso, weekStart = 1) {
  const first = startOfWeek(startOfMonth(iso), weekStart);
  const days = [];
  for (let i = 0; i < 42; i++) days.push(addDays(first, i));
  // A month needs 6 rows only when it spills; trim the trailing all-outside week.
  const last = endOfMonth(iso);
  while (days.length > 35 && days[35] > last) days.length = 35;
  return days;
}

/** Weekday column order for a header row, honouring weekStart. */
export function weekdayOrder(weekStart = 1) {
  const out = [];
  for (let i = 0; i < 7; i++) out.push((weekStart + i) % 7);
  return out;
}

// ---------------------------------------------------------------------------
// Instants
// ---------------------------------------------------------------------------

export function nowDate() {
  return new Date();
}

/** Today's date in the family timezone. */
export function todayISO() {
  const p = wallParts(new Date());
  return fromParts(p.year, p.month, p.day);
}

/** Current wall-clock time in the family timezone, as 'HH:MM'. */
export function nowHM() {
  const p = wallParts(new Date());
  return `${pad2(p.hour)}:${pad2(p.minute)}`;
}

/** Family-timezone date of a UTC instant — this is the bucketing primitive. */
export function instantDate(instant) {
  const p = wallParts(new Date(instant));
  return fromParts(p.year, p.month, p.day);
}

/** Minutes since family-timezone midnight, for ordering within a day. */
export function instantMinutes(instant) {
  const p = wallParts(new Date(instant));
  return p.hour * 60 + p.minute;
}

/** Wall clock of an instant: {date: 'YYYY-MM-DD', time: 'HH:MM'}. */
export function instantToWall(instant) {
  const p = wallParts(new Date(instant));
  return { date: fromParts(p.year, p.month, p.day), time: `${pad2(p.hour)}:${pad2(p.minute)}` };
}

/**
 * Family-timezone wall clock -> UTC instant string.
 * Two passes: the first guess uses the offset at the naive instant, the second
 * uses the offset at the corrected one, which is what makes DST changeovers land
 * on the right side of the transition.
 *
 * A third pass, only ever taken by a wall time the clocks skipped: correcting one
 * of those can cross midnight, and where it does the answer is the other reading.
 * A zone that jumps at midnight has no 00:30 on the day it jumps, and an event
 * typed onto that day must not be saved onto the evening before it. The server
 * resolves a broken hour through the same rule (domain.Date.at) and the two halves
 * of the application must not disagree about one — see docs/architecture.md.
 */
export function wallToInstant(dateISO, hhmm) {
  const { y, m, d } = parseDate(dateISO);
  const [hh, mm] = String(hhmm || '00:00').split(':').map(Number);
  const wanted = fromParts(y, m, d);
  const onWantedDay = (t) => {
    const p = wallParts(new Date(t));
    return fromParts(p.year, p.month, p.day) === wanted;
  };
  const naive = Date.UTC(y, m - 1, d, hh || 0, mm || 0, 0);
  let ms = naive - offsetMs(new Date(naive));
  ms = naive - offsetMs(new Date(ms));
  if (!onWantedDay(ms)) {
    const alt = naive - offsetMs(new Date(ms));
    if (onWantedDay(alt)) ms = alt;
  }
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z');
}

// ---------------------------------------------------------------------------
// Formatting (catalog names, never Intl locale strings, so FR/EN follow the
// user's chosen app language rather than the browser's)
// ---------------------------------------------------------------------------

/**
 * '16:30' or '4:30 PM' depending on the user's time_format.
 * The 12-hour branch needs Intl for the AM/PM marker; it formats a synthetic
 * instant that carries only the wall clock, so `timeZone: 'UTC'` here is the
 * identity transform — the family timezone was already applied upstream.
 */
export function formatHM(hh, mm) {
  if (!is12h()) return `${pad2(hh)}:${pad2(mm)}`;
  const d = new Date(Date.UTC(2000, 0, 1, hh, mm));
  return new Intl.DateTimeFormat(currentLang(), {
    timeZone: 'UTC', hour: 'numeric', minute: '2-digit', hour12: true,
  }).format(d);
}

/** Time of an instant, in the family timezone. */
export function formatTime(instant) {
  const p = wallParts(new Date(instant));
  return formatHM(p.hour, p.minute);
}

/** 'HH:MM' (a stored preference) rendered per the user's time format. */
export function formatClock(hhmm) {
  const [hh, mm] = String(hhmm || '00:00').split(':').map(Number);
  return formatHM(hh || 0, mm || 0);
}

/** 'mardi 4 août 2026' */
export function formatDateLong(iso) {
  const { y, m, d } = parseDate(iso);
  return `${weekdayName(weekdayOf(iso))} ${d} ${monthName(m)} ${y}`;
}

/** 'mar. 4 août' */
export function formatDateMedium(iso) {
  const { m, d } = parseDate(iso);
  return `${weekdayName(weekdayOf(iso), 'short')} ${d} ${monthName(m, true)}`;
}

/** '4 août 2026' */
export function formatDateShort(iso) {
  const { y, m, d } = parseDate(iso);
  return `${d} ${monthName(m, true)} ${y}`;
}

/** 'août 2026', capitalized for a screen title. */
export function formatMonthTitle(iso) {
  const { y, m } = parseDate(iso);
  return `${capitalize(monthName(m))} ${y}`;
}

/** 'Aujourd'hui' / 'Demain' / 'Hier', or null when the date is further away. */
export function relativeDayName(iso, today = todayISO()) {
  const delta = diffDays(today, iso);
  if (delta === 0) return t('date.today');
  if (delta === 1) return t('date.tomorrow');
  if (delta === -1) return t('date.yesterday');
  return null;
}

/** Day heading used by agenda/week/day sheets. */
export function dayHeading(iso, today = todayISO()) {
  const rel = relativeDayName(iso, today);
  const base = capitalize(formatDateMedium(iso));
  return rel ? `${rel} · ${base}` : base;
}

export function isToday(iso, today = todayISO()) {
  return iso === today;
}

export function isWeekend(iso) {
  const w = weekdayOf(iso);
  return w === 0 || w === 6;
}

// ---------------------------------------------------------------------------
// Occurrences
// ---------------------------------------------------------------------------

const MAX_SPAN_DAYS = 366;

/** First family-timezone date an occurrence touches. */
export function occStartDate(occ) {
  return occ.all_day ? occ.start_date : instantDate(occ.starts_at);
}

/**
 * Last family-timezone date an occurrence touches. All-day end_date is inclusive;
 * a timed event ending exactly at midnight belongs to the previous day.
 */
export function occEndDate(occ) {
  if (occ.all_day) return occ.end_date || occ.start_date;
  const start = instantDate(occ.starts_at);
  if (!occ.ends_at) return start;
  let end = instantDate(occ.ends_at);
  if (end > start && instantMinutes(occ.ends_at) === 0) end = addDays(end, -1);
  return end < start ? start : end;
}

/** Every date an occurrence covers, in order. */
export function occDays(occ) {
  const start = occStartDate(occ);
  const end = occEndDate(occ);
  const out = [start];
  let cur = start;
  let guard = 0;
  while (cur < end && guard++ < MAX_SPAN_DAYS) {
    cur = addDays(cur, 1);
    out.push(cur);
  }
  return out;
}

export function isMultiDay(occ) {
  return occStartDate(occ) !== occEndDate(occ);
}

/** true when the chip should read as a bar rather than a timed entry. */
export function isBar(occ) {
  return Boolean(occ.all_day) || isMultiDay(occ);
}

/** Sort: bars first, then by start time, then by title. */
export function compareOccurrences(a, b) {
  const ab = isBar(a) ? 0 : 1;
  const bb = isBar(b) ? 0 : 1;
  if (ab !== bb) return ab - bb;
  if (ab === 0) {
    const as = occStartDate(a);
    const bs = occStartDate(b);
    if (as !== bs) return as < bs ? -1 : 1;
  } else {
    const am = instantMinutes(a.starts_at);
    const bm = instantMinutes(b.starts_at);
    if (am !== bm) return am - bm;
  }
  return String(a.title).localeCompare(String(b.title), currentLang());
}

/** Human time range of an occurrence, for lists and the detail screen. */
export function occTimeLabel(occ) {
  if (isBar(occ)) return t('date.allDay');
  const start = formatTime(occ.starts_at);
  if (!occ.ends_at || occ.ends_at === occ.starts_at) return start;
  return `${start} – ${formatTime(occ.ends_at)}`;
}
