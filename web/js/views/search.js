// Search: keyword + participant + label filters.
// The server does the matching (accent-insensitive LIKE); this screen only
// composes the query and renders results as event rows.

import { h, clear } from '../dom.js';
import { t } from '../i18n.js';
import { icon } from '../icons.js';
import { api } from '../api.js';
import { state, allMembers, calendarById, labelById, labelsOf } from '../state.js';
import { formatDateShort } from '../dates.js';
import { eventRowWithDate } from '../eventui.js';
import { select, emptyState, errorBox, spinner } from '../ui.js';

const DEBOUNCE_MS = 300;

/** Search returns Events, not Occurrences: fill in what the row renderer needs. */
function asOccurrence(ev, nextDate) {
  const cal = calendarById(ev.calendar_id);
  const label = labelById(ev.calendar_id, ev.label_id);
  return {
    event_id: ev.id,
    calendar_id: ev.calendar_id,
    calendar_color: cal ? cal.color : null,
    title: ev.title,
    all_day: ev.all_day,
    starts_at: ev.starts_at,
    ends_at: ev.ends_at,
    start_date: ev.start_date,
    end_date: ev.end_date,
    occurrence_date: nextDate || ev.start_date || (ev.starts_at ? ev.starts_at.slice(0, 10) : ''),
    location: ev.location,
    label_id: ev.label_id,
    label_color: label ? label.color : null,
    label_name: label ? label.name : null,
    participants: ev.participants || [],
  };
}

export function renderSearch() {
  const query = { q: '', participant: '', label_id: '', calendar_id: '' };
  const results = h('div', { class: 'search-results' });
  let timer = null;
  let seq = 0;

  const run = async () => {
    const mine = ++seq;
    const params = {};
    if (query.q.trim()) params.q = query.q.trim();
    if (query.participant) params.participant = query.participant;
    if (query.label_id) params.label_id = query.label_id;
    if (query.calendar_id) params.calendar_id = query.calendar_id;

    if (!Object.keys(params).length) {
      clear(results);
      return;
    }
    clear(results);
    results.appendChild(spinner());
    try {
      const data = await api.search(params);
      if (mine !== seq) return;
      clear(results);
      const list = Array.isArray(data.results) ? data.results : [];
      if (!list.length) {
        results.appendChild(emptyState(t('search.noResults')));
        return;
      }
      const box = h('div', { class: 'event-list' });
      for (const r of list) {
        const occ = asOccurrence(r.event, r.next_occurrence);
        box.appendChild(eventRowWithDate(occ, r.next_occurrence ? formatDateShort(r.next_occurrence) : ''));
      }
      results.appendChild(box);
    } catch (err) {
      if (mine !== seq) return;
      clear(results);
      results.appendChild(errorBox(err, run));
    }
  };

  const schedule = () => {
    if (timer) clearTimeout(timer);
    timer = setTimeout(run, DEBOUNCE_MS);
  };

  const input = h('input', {
    class: 'input search-input',
    type: 'search',
    placeholder: t('search.placeholder'),
    enterkeyhint: 'search',
    autocapitalize: 'off',
    oninput: (e) => { query.q = e.target.value; schedule(); },
  });

  const participantSelect = select(
    [{ value: '', label: t('search.all') }].concat(allMembers().map((m) => ({ value: m.user_id, label: m.display_name }))),
    { value: '', onchange: (e) => { query.participant = e.target.value; run(); } });

  const labelOptions = [{ value: '', label: t('search.all') }];
  for (const cal of state.calendars) {
    for (const l of labelsOf(cal.id)) {
      labelOptions.push({ value: l.id, label: `${cal.name} · ${l.name}` });
    }
  }
  const labelSelect = select(labelOptions, {
    value: '',
    onchange: (e) => {
      query.label_id = e.target.value;
      // A label belongs to one calendar; scope the query so ids cannot collide.
      const chosen = state.calendars.find((c) => labelsOf(c.id).some((l) => String(l.id) === e.target.value));
      query.calendar_id = chosen ? chosen.id : '';
      run();
    },
  });

  return h('div', { class: 'search screen' },
    h('header', { class: 'screen-bar' },
      h('h1', { class: 'screen-title' }, t('nav.search'))),
    h('div', { class: 'search-controls' },
      h('div', { class: 'search-field' }, icon('search'), input),
      h('div', { class: 'search-filters' },
        h('label', { class: 'filter' }, h('span', null, t('search.participant')), participantSelect),
        h('label', { class: 'filter' }, h('span', null, t('search.label')), labelSelect))),
    h('div', { class: 'screen-body scroll' }, results));
}
