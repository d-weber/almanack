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

/**
 * Search returns Events, not Occurrences: fill in what the row renderer needs.
 *
 * The occurrence date is the one field with no answer to copy, and this screen no longer
 * works it out. It used to try three sources in turn — the next occurrence, the event's
 * own start date, the day its start instant falls on — and the last two are guesses that
 * only ever ran for a series that has ended, which is exactly when they are wrong: an
 * anchor need not be an occurrence of the rule it anchors. The server expands the rule
 * anyway and now says which day the row stands for, so there is one answer instead of a
 * chain, computed where the rule lives.
 *
 * It is empty only for a rule with no occurrence at all, which no date could open.
 */
function asOccurrence(ev, occurrenceDate) {
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
    occurrence_date: occurrenceDate || '',
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
        const occ = asOccurrence(r.event, r.occurrence_date);
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
