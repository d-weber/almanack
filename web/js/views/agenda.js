// Agenda view: one continuous list from today forward, grouped by day with sticky
// headers. Days with nothing on them are skipped — this screen answers "what is
// coming up", not "what does the month look like".
//
// It keeps its own list rather than the shared range cache, so scrolling a year
// ahead never evicts the month the user came from.

import { h, clear } from '../dom.js';
import { t } from '../i18n.js';
import { todayISO, addDays, occDays, compareOccurrences, dayHeading, formatDateShort } from '../dates.js';
import { fetchRange, isVisible } from '../state.js';
import { eventRow } from '../eventui.js';
import { spinner, emptyState, errorBox } from '../ui.js';

const CHUNK_DAYS = 45;
const MAX_CHUNKS = 9; // ~400 days ahead, then the list stops growing

export function renderAgenda({ date }) {
  const today = todayISO();
  const start = date || today;

  const list = h('div', { class: 'agenda-list' });
  const footer = h('div', { class: 'agenda-footer' });
  const sentinel = h('div', { class: 'agenda-sentinel' });
  const wrap = h('div', { class: 'agenda' }, list, footer);

  let cursor = start;
  let chunks = 0;
  let loading = false;
  let done = false;
  let anyEvent = false;
  let observer = null;

  function renderChunk(from, to, occurrences, holidays) {
    const byDate = new Map();
    for (const occ of occurrences) {
      if (!isVisible(occ.calendar_id)) continue;
      for (const day of occDays(occ)) {
        // Clip to this chunk so a multi-day event never creates the same day
        // section twice across two loads.
        if (day < from || day > to) continue;
        let arr = byDate.get(day);
        if (!arr) byDate.set(day, (arr = []));
        arr.push(occ);
      }
    }
    const holidayMap = new Map((holidays || []).map((hd) => [hd.date, hd.name]));
    for (const day of Array.from(byDate.keys()).sort()) {
      const items = byDate.get(day).sort(compareOccurrences);
      anyEvent = true;
      list.appendChild(h('section', { class: ['agenda-day', day === today ? 'is-today' : ''].filter(Boolean).join(' ') },
        h('h2', { class: 'agenda-day-head' },
          h('span', { class: 'agenda-day-title' }, dayHeading(day, today)),
          holidayMap.get(day) ? h('span', { class: 'agenda-holiday' }, holidayMap.get(day)) : null),
        h('div', { class: 'event-list' }, ...items.map((occ) => eventRow(occ)))));
    }
  }

  async function loadMore() {
    if (loading || done) return;
    loading = true;
    clear(footer);
    footer.appendChild(spinner());

    const from = cursor;
    const to = addDays(cursor, CHUNK_DAYS - 1);
    try {
      const data = await fetchRange(from, to);
      renderChunk(from, to, data.occurrences, data.holidays);
      cursor = addDays(to, 1);
      chunks += 1;
      clear(footer);
      if (!anyEvent) footer.appendChild(emptyState(t('event.noEvents')));
      if (chunks >= MAX_CHUNKS) {
        done = true;
        if (observer) observer.disconnect();
      } else {
        footer.appendChild(sentinel);
      }
    } catch (err) {
      clear(footer);
      footer.appendChild(errorBox(err, () => { done = false; loadMore(); }));
      done = true;
      if (observer) observer.disconnect();
    } finally {
      loading = false;
    }
  }

  if ('IntersectionObserver' in window) {
    observer = new IntersectionObserver((entries) => {
      for (const e of entries) if (e.isIntersecting) loadMore();
    }, { rootMargin: '600px' });
    observer.observe(sentinel);
  } else {
    sentinel.appendChild(h('button', {
      class: 'btn btn-quiet btn-wide',
      type: 'button',
      onclick: () => loadMore(),
    }, t('action.loading')));
  }

  // The list stops paging on its own at MAX_CHUNKS and disconnects there. Leaving the
  // screen first is the other way it ends, and only app.js knows about that one.
  wrap.cleanup = () => { if (observer) observer.disconnect(); };

  loadMore();
  return wrap;
}

/** Header subtitle for the agenda screen. */
export function agendaTitle(dateISO) {
  return formatDateShort(dateISO || todayISO());
}
