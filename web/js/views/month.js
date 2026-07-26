// Month view: the default screen.
//
// Each week is one row containing, in this order: a full-height tap layer of seven
// day buttons, the day numbers with any French public holiday, spanning bars for
// all-day and multi-day events, then the timed chips per day. Bars are laid out in
// lanes so overlapping holidays and trips never hide each other.

import { h } from '../dom.js';
import { t, weekdayName } from '../i18n.js';
import { icon } from '../icons.js';
import {
  monthGrid, weekdayOrder, diffDays, addDays, todayISO, parseDate,
  isBar, occStartDate, occEndDate, dayHeading,
  startOfMonth, endOfMonth, formatDateLong,
} from '../dates.js';
import { occurrencesOn, holidayOn, weekStart } from '../state.js';
import { eventChip, eventBar, eventRow } from '../eventui.js';
import { openOverlay, button, emptyState } from '../ui.js';
import { go } from '../router.js';

// A cell shows at most this many timed events; the rest collapse into a "+N"
// button that opens the day sheet. The row clips, so this is also what keeps a
// busy Wednesday from bleeding into the next week.
const MAX_CHIPS_PER_DAY = 3;

/** The window the month screen must have loaded: the whole 5–6 week grid. */
export function monthRange(dateISO, ws = 1) {
  const days = monthGrid(dateISO, ws);
  return { from: days[0], to: days[days.length - 1] };
}

function barsForWeek(days) {
  const first = days[0];
  const last = days[days.length - 1];
  const seen = new Map();
  for (const day of days) {
    for (const occ of occurrencesOn(day)) {
      if (!isBar(occ)) continue;
      const key = `${occ.event_id}:${occ.occurrence_date}`;
      if (!seen.has(key)) seen.set(key, occ);
    }
  }
  const items = Array.from(seen.values()).map((occ) => {
    const s = occStartDate(occ);
    const e = occEndDate(occ);
    return {
      occ,
      startCol: Math.max(0, diffDays(first, s)),
      endCol: Math.min(days.length - 1, diffDays(first, e)),
      isStart: s >= first,
      isEnd: e <= last,
    };
  }).sort((a, b) => (a.startCol - b.startCol) || ((b.endCol - b.startCol) - (a.endCol - a.startCol)));

  const lanes = [];
  for (const it of items) {
    let lane = 0;
    while (lanes[lane] && lanes[lane].some(([s, e]) => !(it.endCol < s || it.startCol > e))) lane += 1;
    if (!lanes[lane]) lanes[lane] = [];
    lanes[lane].push([it.startCol, it.endCol]);
    it.lane = lane;
  }
  return items;
}

function dayCell(day, monthNum, today) {
  const { m } = parseDate(day);
  const holiday = holidayOn(day);
  const classes = ['day-hit'];
  if (m !== monthNum) classes.push('is-outside');
  if (day === today) classes.push('is-today');
  if (holiday) classes.push('is-holiday');
  return h('button', {
    class: classes.join(' '),
    type: 'button',
    'aria-label': formatDateLong(day),
    onclick: () => openDaySheet(day),
  });
}

export function renderMonth({ date }) {
  const ws = weekStart();
  const today = todayISO();
  const anchor = date || today;
  const monthNum = parseDate(anchor).m;
  const days = monthGrid(anchor, ws);

  const head = h('div', { class: 'month-weekdays' },
    ...weekdayOrder(ws).map((dow) => h('span', {
      class: ['month-weekday', dow === 0 || dow === 6 ? 'is-weekend' : ''].filter(Boolean).join(' '),
      title: weekdayName(dow),
    }, weekdayName(dow, 'narrow'))));

  const grid = h('div', { class: 'month-grid' });

  for (let i = 0; i < days.length; i += 7) {
    const week = days.slice(i, i + 7);
    const bars = barsForWeek(week);
    const laneCount = bars.reduce((n, b) => Math.max(n, b.lane + 1), 0);

    const row = h('div', { class: 'week-row' },
      h('div', { class: 'week-hit' }, ...week.map((day) => dayCell(day, monthNum, today))),
      h('div', { class: 'week-content' },
        h('div', { class: 'week-nums' }, ...week.map((day) => {
          const { m, d } = parseDate(day);
          const holiday = holidayOn(day);
          return h('span', { class: ['week-num', m !== monthNum ? 'is-outside' : '', day === today ? 'is-today' : ''].filter(Boolean).join(' ') },
            h('span', { class: 'num' }, String(d)),
            holiday ? h('span', { class: 'holiday-name', title: holiday }, holiday) : null);
        })),
        laneCount
          ? h('div', { class: 'week-bars', style: { '--lanes': String(laneCount) } },
            ...bars.map((b) => {
              const node = eventBar(b.occ, { isStart: b.isStart, isEnd: b.isEnd });
              node.style.setProperty('grid-column', `${b.startCol + 1} / span ${b.endCol - b.startCol + 1}`);
              node.style.setProperty('grid-row', String(b.lane + 1));
              return node;
            }))
          : null,
        h('div', { class: 'week-chips' }, ...week.map((day) => {
          const timed = occurrencesOn(day).filter((occ) => !isBar(occ));
          const shown = timed.slice(0, MAX_CHIPS_PER_DAY);
          const rest = timed.length - shown.length;
          return h('div', { class: 'day-chips' },
            ...shown.map((occ) => eventChip(occ)),
            rest > 0
              ? h('button', { class: 'chip-more', type: 'button', onclick: () => openDaySheet(day) }, `+${rest}`)
              : null);
        }))));
    grid.appendChild(row);
  }

  return h('div', { class: 'month' }, head, grid);
}

/** Day sheet: everything on one day, plus a shortcut to add to it. */
export function openDaySheet(dateISO) {
  const today = todayISO();
  const list = occurrencesOn(dateISO);
  const holiday = holidayOn(dateISO);

  openOverlay((close) => h('div', { class: 'day-sheet' },
    h('div', { class: 'sheet-head' },
      h('div', null,
        h('h2', { class: 'sheet-title' }, dayHeading(dateISO, today)),
        holiday ? h('p', { class: 'sheet-sub' }, holiday) : null),
      h('button', { class: 'icon-btn', type: 'button', 'aria-label': t('action.close'), onclick: close }, icon('close'))),
    h('div', { class: 'sheet-body scroll' },
      list.length
        ? h('div', { class: 'event-list' }, ...list.map((occ) => eventRow(occ, {
          onclick: () => { close(); go(`/event/${occ.event_id}/${occ.occurrence_date}`); },
        })))
        : emptyState(dateISO === today ? t('event.noEventsToday') : t('event.noEvents'))),
    h('div', { class: 'sheet-actions' },
      button(t('action.add'), {
        iconName: 'plus',
        wide: true,
        onclick: () => { close(); go(`/event/new?date=${dateISO}`); },
      }))), { variant: 'sheet' });
}

/** Helpers re-exported for the shell's navigation arrows. */
export function prevMonthAnchor(dateISO) {
  return startOfMonth(addDays(startOfMonth(dateISO), -1));
}

export function nextMonthAnchor(dateISO) {
  return addDays(endOfMonth(dateISO), 1);
}
