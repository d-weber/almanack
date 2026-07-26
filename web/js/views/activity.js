// Activity: the change feed, grouped by day.
// Entries are denormalized server-side (the title survives the event's deletion),
// so a deleted event still reads correctly here.

import { h, clear } from '../dom.js';
import { t } from '../i18n.js';
import { api } from '../api.js';
import { memberById, calendarById } from '../state.js';
import { instantDate, formatTime, dayHeading, todayISO } from '../dates.js';
import { avatar, emptyState, errorBox, spinner, button } from '../ui.js';
import { go } from '../router.js';

const PAGE = 50;
const MAX_PAGES = 10;

function actionText(entry) {
  const user = memberById(entry.user_id);
  const name = user ? user.display_name : '';
  const key = `activity.${entry.action}`;
  return t(key, { user: name, title: entry.title || '' });
}

export function renderActivity() {
  const list = h('div', { class: 'activity-list' });
  const footer = h('div', { class: 'activity-footer' });
  const today = todayISO();

  const sentinel = h('div', { class: 'activity-sentinel' });
  let before = null;
  let pages = 0;
  let loading = false;
  let done = false;
  let any = false;
  let lastDay = null;
  let observer = null;

  const append = (entries) => {
    for (const entry of entries) {
      const day = instantDate(entry.at);
      if (day !== lastDay) {
        lastDay = day;
        list.appendChild(h('h2', { class: 'activity-day' }, dayHeading(day, today)));
      }
      const actor = memberById(entry.user_id);
      const cal = calendarById(entry.calendar_id);
      const meta = [formatTime(entry.at)];
      if (cal) meta.push(cal.name);
      const row = h('div', { class: 'activity-item' },
        avatar(actor, 'sm'),
        h('div', { class: 'activity-body' },
          h('p', { class: 'activity-text' }, actionText(entry)),
          h('p', { class: 'activity-meta' }, meta.join(' · '))));
      if (entry.event_id && entry.action !== 'event_deleted') {
        list.appendChild(h('button', {
          class: 'activity-row',
          type: 'button',
          onclick: () => go(`/event/${entry.event_id}/${day}`),
        }, row));
      } else {
        list.appendChild(h('div', { class: 'activity-row' }, row));
      }
      any = true;
    }
  };

  const load = async () => {
    if (loading || done) return;
    loading = true;
    clear(footer);
    footer.appendChild(spinner());
    try {
      const data = await api.activity({ limit: PAGE, before: before || undefined });
      const entries = Array.isArray(data.activity) ? data.activity : [];
      append(entries);
      pages += 1;
      clear(footer);
      if (entries.length) before = entries[entries.length - 1].at;
      if (!entries.length || entries.length < PAGE || pages >= MAX_PAGES) {
        done = true;
        if (observer) observer.disconnect();
        if (!any) footer.appendChild(emptyState(t('activity.empty')));
      } else {
        footer.appendChild(sentinel);
      }
    } catch (err) {
      clear(footer);
      footer.appendChild(errorBox(err, () => { done = false; load(); }));
      done = true;
      if (observer) observer.disconnect();
    } finally {
      loading = false;
    }
  };

  if ('IntersectionObserver' in window) {
    observer = new IntersectionObserver((entries) => {
      for (const e of entries) if (e.isIntersecting) load();
    }, { rootMargin: '400px' });
    observer.observe(sentinel);
  } else {
    sentinel.appendChild(button(t('action.loading'), { variant: 'quiet', wide: true, onclick: load }));
  }

  load();

  return h('div', { class: 'activity screen' },
    h('header', { class: 'screen-bar' },
      h('h1', { class: 'screen-title' }, t('activity.title'))),
    h('div', { class: 'screen-body scroll' }, list, footer));
}
