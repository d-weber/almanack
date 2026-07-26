// Occurrence chips, bars and rows — the shared vocabulary of the month, week,
// agenda, day-sheet and search screens.
//
// Colour reading (see colors.js): the body colour follows the current mode, and
// the leading strip always carries the participants' colours, so "whose event is
// this" is answered before the title is read.

import { h } from './dom.js';
import { t } from './i18n.js';
import { icon } from './icons.js';
import { state, participantsOf, calendarById } from './state.js';
import { occurrenceColors, chipStyle } from './colors.js';
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
  h('span', { class: 'chip-strip', style: { '--c-strip': colors.strip } }),
  timed ? h('span', { class: 'chip-time' }, formatTime(occ.starts_at)) : null,
  h('span', { class: 'chip-title' }, occ.title),
  labelDot(occ, colors));
}

/** Spanning bar for an all-day or multi-day event inside a week row. */
export function eventBar(occ, { isStart = true, isEnd = true, onclick } = {}) {
  const colors = occColors(occ);
  return h('button', {
    class: ['bar', isStart ? 'is-start' : '', isEnd ? 'is-end' : ''].filter(Boolean).join(' '),
    type: 'button',
    style: chipStyle(colors),
    onclick: onclick || (() => openOccurrence(occ)),
  },
  h('span', { class: 'bar-strip', style: { '--c-strip': colors.strip } }),
  h('span', { class: 'bar-title' }, isStart ? occ.title : ''),
  labelDot(occ, colors));
}

/** Full-width row for the week, agenda, day sheet and search screens. */
export function eventRow(occ, { showCalendar = true, onclick } = {}) {
  const colors = occColors(occ);
  const people = participantsOf(occ);
  const cal = calendarById(occ.calendar_id);
  const meta = [];
  if (showCalendar && cal) meta.push(cal.name);
  if (occ.location) meta.push(occ.location);

  return h('button', {
    class: 'event-row',
    type: 'button',
    style: chipStyle(colors),
    onclick: onclick || (() => openOccurrence(occ)),
  },
  h('span', { class: 'event-strip', style: { '--c-strip': colors.strip } }),
  h('span', { class: 'event-time' }, isBar(occ) ? t('date.allDay') : formatTime(occ.starts_at)),
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
