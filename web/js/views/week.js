// Week view: the seven days as sections, each listing its events in order.
// Deliberately not an hourly grid — a family reads "what is on Wednesday", and the
// hourly view is the one thing TimeTree itself paywalls.

import { h } from '../dom.js';
import { t } from '../i18n.js';
import { icon } from '../icons.js';
import { startOfWeek, addDays, todayISO, dayHeading } from '../dates.js';
import { occurrencesOn, holidayOn, weekStart } from '../state.js';
import { eventRow } from '../eventui.js';
import { emptyState } from '../ui.js';
import { go } from '../router.js';

/** The window the week screen must have loaded. */
export function weekRange(dateISO, ws = 1) {
  const from = startOfWeek(dateISO, ws);
  return { from, to: addDays(from, 6) };
}

export function renderWeek({ date }) {
  const ws = weekStart();
  const today = todayISO();
  const first = startOfWeek(date || today, ws);
  const wrap = h('div', { class: 'week-view' });

  for (let i = 0; i < 7; i++) {
    const day = addDays(first, i);
    const list = occurrencesOn(day);
    const holiday = holidayOn(day);
    wrap.appendChild(h('section', { class: ['week-day', day === today ? 'is-today' : ''].filter(Boolean).join(' ') },
      h('header', { class: 'week-day-head' },
        h('h2', { class: 'week-day-title' }, dayHeading(day, today)),
        holiday ? h('span', { class: 'week-day-holiday' }, holiday) : null,
        h('button', {
          class: 'icon-btn icon-btn-quiet',
          type: 'button',
          'aria-label': t('action.add'),
          onclick: () => go(`/event/new?date=${day}`),
        }, icon('plus'))),
      list.length
        ? h('div', { class: 'event-list' }, ...list.map((occ) => eventRow(occ)))
        : emptyState(day === today ? t('event.noEventsToday') : t('event.noEvents'))));
  }

  return wrap;
}
