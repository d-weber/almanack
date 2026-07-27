// Occurrence chips, bars and rows — the shared vocabulary of the month, week,
// agenda, day-sheet and search screens.
//
// Two shapes, because the two kinds of event answer different questions. An
// all-day event owns the day, so it is drawn as a filled box in its own colour.
// A timed event is one moment in a day that holds several, so on a wide screen it
// is a dot, the title, and the hour — the hour aligned down the column so a day
// can be scanned vertically. A phone has no room for that column: there the title
// takes the colour and sits on a tint of it, which reads at a glance without
// spending characters on digits.

import { h } from './dom.js';
import { t } from './i18n.js';
import { icon } from './icons.js';
import { state, participantsOf, calendarById } from './state.js';
import { occurrenceColors, chipStyle, normalizeHex } from './colors.js';
import { formatTime, isBar, occStartDate, occEndDate } from './dates.js';
import { avatar } from './ui.js';
import { go } from './router.js';

export function occColors(occ) {
  return occurrenceColors(occ, state.colorBy, participantsOf(occ));
}

export function eventHref(occ) {
  return `#/event/${occ.event_id}/${occ.occurrence_date}`;
}

export function openOccurrence(occ) {
  go(`/event/${occ.event_id}/${occ.occurrence_date}`);
}

function labelDot(occ, colors) {
  if (state.colorBy !== 'person' || !occ.label_id) return null;
  return h('span', { class: 'event-label-dot', style: { '--c-label': colors.label }, title: occ.label_name || '' });
}

/**
 * Public holidays belong to no calendar and carry no label, so they have no
 * colour of their own to take. The operator picks one (ALMANACK_HOLIDAY_COLOR);
 * red is the default because that is what a wall calendar does.
 */
export function holidayColor() {
  return normalizeHex(state.config && state.config.holiday_color, '#d32f2f');
}

/** A holiday drawn like the all-day event it effectively is. */
export function holidayBar(name) {
  const c = holidayColor();
  return h('div', {
    class: 'bar is-start is-end is-holiday-bar',
    style: chipStyle({ main: c, label: c }),
    title: name,
  }, h('span', { class: 'bar-title' }, name));
}

/** Compact chip for a month cell. */
export function eventChip(occ, { onclick } = {}) {
  const colors = occColors(occ);
  const timed = !isBar(occ);
  return h('button', {
    class: ['chip', timed ? 'chip-timed' : 'chip-bar'].join(' '),
    type: 'button',
    style: chipStyle(colors),
    onclick: onclick || (() => openOccurrence(occ)),
  },
  timed ? h('span', { class: 'chip-dot' }) : null,
  h('span', { class: 'chip-title' }, occ.title),
  timed ? h('span', { class: 'chip-time' }, formatTime(occ.starts_at)) : null);
}

/** Spanning bar for an all-day or multi-day event inside a week row. */
export function eventBar(occ, { isStart = true, isEnd = true, onclick } = {}) {
  const colors = occColors(occ);
  return h('button', {
    class: ['bar', isStart ? 'is-start' : '', isEnd ? 'is-end' : ''].filter(Boolean).join(' '),
    type: 'button',
    style: chipStyle(colors),
    // The title is written once, in the week the event begins, because a 20px bar has
    // room for it once. Every segment is a button of its own, though, so a segment
    // without a name of its own is announced as "button" and says nothing on hover —
    // which is what a week-long holiday looked like from its second week onwards.
    title: occ.title,
    'aria-label': occ.title,
    onclick: onclick || (() => openOccurrence(occ)),
  },
  h('span', { class: 'bar-title' }, isStart ? occ.title : ''));
}

/** Full-width row for the week, agenda, day sheet and search screens. */
export function eventRow(occ, { showCalendar = true, onclick } = {}) {
  const colors = occColors(occ);
  const people = participantsOf(occ);
  const cal = calendarById(occ.calendar_id);
  const meta = [];
  if (showCalendar && cal) meta.push(cal.name);
  if (occ.location) meta.push(occ.location);

  const allDay = isBar(occ);
  return h('button', {
    class: ['event-row', allDay ? 'is-allday' : 'is-timed'].join(' '),
    type: 'button',
    style: chipStyle(colors),
    onclick: onclick || (() => openOccurrence(occ)),
  },
  h('span', { class: 'event-time' }, allDay ? t('date.allDay') : formatTime(occ.starts_at)),
  allDay ? null : h('span', { class: 'chip-dot' }),
  h('span', { class: 'event-body' },
    h('span', { class: 'event-title' }, occ.title),
    meta.length ? h('span', { class: 'event-meta' }, meta.join(' · ')) : null),
  people.length ? h('span', { class: 'event-people' }, ...people.slice(0, 4).map((p) => avatar(p, 'xs'))) : null,
  labelDot(occ, colors));
}

/** Row variant that also states the day — used by search results. */
export function eventRowWithDate(occ, dateLabel) {
  const row = eventRow(occ, { showCalendar: true });
  const body = row.querySelector('.event-body');
  if (body) body.appendChild(h('span', { class: 'event-meta' }, dateLabel));
  return row;
}

/** Little clock/pin/repeat markers used on the detail screen. */
export function metaLine(iconName, text, href) {
  if (!text) return null;
  const content = href
    ? h('a', { class: 'meta-link', href, target: '_blank', rel: 'noreferrer noopener' }, text)
    : h('span', null, text);
  return h('p', { class: 'meta-line' }, icon(iconName), content);
}

/** Inclusive day span label for a multi-day event, e.g. '4 – 8 août'. */
export function spanLabel(occ, formatDate) {
  const a = occStartDate(occ);
  const b = occEndDate(occ);
  return a === b ? formatDate(a) : `${formatDate(a)} – ${formatDate(b)}`;
}
