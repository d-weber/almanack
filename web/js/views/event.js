// Event detail and event editor.
//
// Two screens live here because they share the whole occurrence vocabulary:
//   #/event/:id/:date        detail (read)
//   #/event/:id/:date/edit   editor (update)
//   #/event/new?date=…       editor (create)
//
// Recurrence rule: any save or delete that touches an occurrence of a series asks
// the scope question first (This / This and following / The whole series) and
// passes both `scope` and `date` to the API, exactly as the contract requires.

import { h, clear } from '../dom.js';
import { t, weekdayName } from '../i18n.js';
import { icon } from '../icons.js';
import { api } from '../api.js';
import {
  todayISO, addDays, parseDate, daysInMonth, weekdayOf, wallToInstant, instantToWall,
  formatDateLong, formatDateShort, formatClock, occTimeLabel, isBar,
  occStartDate, occEndDate, weekdayOrder,
} from '../dates.js';
import {
  state, calendarById, labelsOf, membersOf, participantsOf, memberById,
  invalidateRange, weekStart, normalizeOccurrence,
} from '../state.js';
import {
  screen, section, field, input, textarea, select, button, toggleRow, memberSelector,
  avatar, chooseDialog, confirmDialog, toast, spinner, errorBox, formError, errorMessage,
} from '../ui.js';
import { normalizeHex, readableOn } from '../colors.js';
import { metaLine } from '../eventui.js';
import { go, back } from '../router.js';

const SCOPE_OPTIONS = () => [
  { value: 'this', label: t('scope.this') },
  { value: 'upcoming', label: t('scope.upcoming') },
  { value: 'all', label: t('scope.all') },
];

const TIMED_PRESETS = [0, 5, 10, 15, 30, 60, 120, 1440, 2880, 10080];
const ALLDAY_PRESETS = [
  { days_before: 0, at_time_local: '09:00' },
  { days_before: 1, at_time_local: '09:00' },
  { days_before: 1, at_time_local: '20:00' },
  { days_before: 2, at_time_local: '09:00' },
  { days_before: 7, at_time_local: '09:00' },
];

function reminderLabel(r, allDay) {
  if (allDay) {
    const time = formatClock(r.at_time_local || '09:00');
    const n = Number(r.days_before || 0);
    if (n === 0) return t('reminder.onDayAt', { time });
    if (n === 1) return t('reminder.dayBeforeAt', { time });
    return t('reminder.daysBeforeAt', { n, time });
  }
  const m = Number(r.offset_minutes || 0);
  if (m === 0) return t('reminder.atStart');
  if (m % 10080 === 0) return t('reminder.weeksBefore', { n: m / 10080 });
  if (m % 1440 === 0) return t('reminder.daysBefore', { n: m / 1440 });
  if (m % 60 === 0) return t('reminder.hoursBefore', { n: m / 60 });
  return t('reminder.minutesBefore', { n: m });
}

function reminderKey(r, allDay) {
  return allDay ? `d${r.days_before}@${r.at_time_local}` : `m${r.offset_minutes}`;
}

/** Human sentence for a recurrence, from the catalog only. */
export function recurrenceSummary(rec) {
  if (!rec || !rec.freq) return t('recur.none');
  const interval = Number(rec.interval || 1);
  const unitKey = { daily: 'days', weekly: 'weeks', monthly: 'months', yearly: 'years' }[rec.freq];
  let base;
  if (interval <= 1) {
    base = t(`recur.${rec.freq}`);
  } else {
    base = `${t('recur.interval')} ${interval} ${t(`recur.interval.${unitKey}`)}`;
  }
  const extra = [];
  if (rec.freq === 'weekly' && Array.isArray(rec.by_weekday) && rec.by_weekday.length) {
    extra.push(rec.by_weekday.map((d) => weekdayName(d, 'short')).join(', '));
  }
  if (rec.freq === 'monthly') {
    if (rec.month_last_day) extra.push(t('recur.monthlyMode.lastDay'));
    else if (rec.week_ordinal && Array.isArray(rec.by_weekday) && rec.by_weekday.length) {
      extra.push(t('recur.monthlyMode.weekday', {
        ordinal: t(`recur.ordinal.${rec.week_ordinal}`),
        weekday: weekdayName(rec.by_weekday[0]),
      }));
    } else if (rec.by_monthday) {
      extra.push(t('recur.monthlyMode.day', { day: rec.by_monthday }));
    }
  }
  if (rec.until) extra.push(`${t('recur.until')} ${formatDateShort(rec.until)}`);
  return extra.length ? `${base} · ${extra.join(' · ')}` : base;
}

// ---------------------------------------------------------------------------
// Detail
// ---------------------------------------------------------------------------

export async function renderEventDetail({ id, date }) {
  const body = h('div', { class: 'event-detail' }, spinner());
  const view = screen({
    title: t('action.loading'),
    onBack: () => back(`/${state.view}?d=${date || todayISO()}`),
    actions: [],
  }, body);

  let data;
  try {
    data = await api.event(id, date);
  } catch (err) {
    clear(body);
    body.appendChild(errorBox(err, () => go(`/event/${id}/${date}`)));
    return view;
  }

  const occ = normalizeOccurrence(data.occurrence);
  const rec = data.recurrence || null;
  const reminders = Array.isArray(data.my_reminders) ? data.my_reminders : [];
  const cal = calendarById(occ.calendar_id);
  const people = participantsOf(occ);
  const creator = memberById(occ.created_by);
  const labelColor = normalizeHex(occ.label_color || occ.calendar_color);

  const title = view.querySelector('.screen-title');
  if (title) clear(title).appendChild(document.createTextNode(cal ? cal.name : t('nav.calendar')));

  const actions = view.querySelector('.screen-actions');
  if (actions) {
    actions.appendChild(h('button', {
      class: 'icon-btn', type: 'button', 'aria-label': t('action.edit'),
      onclick: () => go(`/event/${occ.event_id}/${occ.occurrence_date}/edit`),
    }, icon('pencil')));
  }

  const dateLine = isBar(occ)
    ? (occStartDate(occ) === occEndDate(occ)
      ? formatDateLong(occStartDate(occ))
      : `${formatDateShort(occStartDate(occ))} – ${formatDateShort(occEndDate(occ))}`)
    : formatDateLong(occStartDate(occ));

  clear(body);
  body.appendChild(h('div', {
    class: 'detail-head',
    style: { '--c': labelColor, '--c-on': readableOn(labelColor) },
  },
  h('h1', { class: 'detail-title' }, occ.title),
  h('p', { class: 'detail-when' }, `${dateLine} · ${occTimeLabel(occ)}`)));

  const facts = section(null,
    cal ? metaLine('calendar', cal.name) : null,
    occ.label_name ? metaLine('tag', occ.label_name) : null,
    rec ? metaLine('repeat', recurrenceSummary(rec)) : null,
    occ.location ? metaLine('pin', occ.location) : null,
    occ.url ? metaLine('link', occ.url, occ.url) : null,
    occ.notes ? h('p', { class: 'detail-notes' }, occ.notes) : null);
  body.appendChild(facts);

  if (people.length) {
    body.appendChild(section(t('event.participants'),
      h('div', { class: 'people-list' }, ...people.map((p) => h('span', { class: 'person' }, avatar(p, 'sm'), h('span', null, p.display_name))))));
  }

  body.appendChild(section(t('reminder.title'),
    reminders.length
      ? h('ul', { class: 'plain-list' }, ...reminders.map((r) =>
        h('li', null, reminderLabel({
          offset_minutes: r.offset_minutes,
          days_before: r.days_before,
          at_time_local: r.at_time_local,
        }, occ.all_day))))
      : h('p', { class: 'field-hint' }, t('reminder.none')),
    h('p', { class: 'field-hint' }, t('reminder.hint'))));

  if (creator) body.appendChild(h('p', { class: 'detail-credit' }, t('event.createdBy', { name: creator.display_name })));

  body.appendChild(h('div', { class: 'detail-actions' },
    button(t('action.edit'), { iconName: 'pencil', wide: true, onclick: () => go(`/event/${occ.event_id}/${occ.occurrence_date}/edit`) }),
    button(t('action.delete'), {
      variant: 'danger', iconName: 'trash', wide: true,
      onclick: () => deleteOccurrence(occ, Boolean(rec)),
    })));

  return view;
}

async function deleteOccurrence(occ, isSeries) {
  let scope = null;
  if (isSeries) {
    scope = await chooseDialog(t('scope.deleteTitle'), SCOPE_OPTIONS());
    if (!scope) return;
  } else {
    const ok = await confirmDialog({
      title: t('event.delete.confirm', { title: occ.title }),
      confirmLabel: t('action.delete'),
      danger: true,
    });
    if (!ok) return;
  }
  try {
    await api.deleteEvent(occ.event_id, scope ? { scope, date: occ.occurrence_date } : {});
    invalidateRange();
    go(`/${state.view}?d=${occ.occurrence_date}`);
  } catch (err) {
    toast(errorMessage(err));
  }
}

// ---------------------------------------------------------------------------
// Editor
// ---------------------------------------------------------------------------

function blankForm(presetDate, calendarId) {
  const day = presetDate || todayISO();
  const cal = calendarById(calendarId) || state.calendars[0] || null;
  const labels = cal ? labelsOf(cal.id) : [];
  return {
    id: null,
    calendar_id: cal ? cal.id : null,
    title: '',
    all_day: false,
    start_date: day,
    end_date: day,
    start_time: '09:00',
    end_time: '10:00',
    location: '',
    url: '',
    notes: '',
    label_id: labels.length ? labels[0].id : null,
    participants: state.user ? [state.user.id] : [],
    recurrence: null,
    reminders: [],
    occurrence_date: day,
    is_series: false,
    pristine: null,
  };
}

function formFromOccurrence(occ, rec, reminders) {
  const startWall = occ.all_day ? null : instantToWall(occ.starts_at);
  const endWall = occ.all_day || !occ.ends_at ? null : instantToWall(occ.ends_at);
  return {
    id: occ.event_id,
    calendar_id: occ.calendar_id,
    title: occ.title || '',
    all_day: Boolean(occ.all_day),
    start_date: occ.all_day ? occ.start_date : startWall.date,
    end_date: occ.all_day ? (occ.end_date || occ.start_date) : (endWall ? endWall.date : startWall.date),
    start_time: startWall ? startWall.time : '09:00',
    end_time: endWall ? endWall.time : '10:00',
    location: occ.location || '',
    url: occ.url || '',
    notes: occ.notes || '',
    label_id: occ.label_id || null,
    participants: Array.isArray(occ.participants) ? occ.participants.slice() : [],
    recurrence: rec
      ? {
        freq: rec.freq,
        interval: rec.interval || 1,
        by_weekday: Array.isArray(rec.by_weekday) ? rec.by_weekday.slice() : [],
        by_monthday: rec.by_monthday || null,
        week_ordinal: rec.week_ordinal || null,
        month_last_day: Boolean(rec.month_last_day),
        until: rec.until || null,
      }
      : null,
    reminders: (reminders || []).map((r) => ({
      offset_minutes: r.offset_minutes == null ? undefined : r.offset_minutes,
      days_before: r.days_before == null ? undefined : r.days_before,
      at_time_local: r.at_time_local || undefined,
    })),
    occurrence_date: occ.occurrence_date,
    is_series: Boolean(rec),
    // The instants this form was built from, beside the wall clock they produced. See
    // instantFor(): once a year that wall clock does not name them back.
    pristine: {
      start: startWall ? { at: occ.starts_at, date: startWall.date, time: startWall.time } : null,
      end: endWall ? { at: occ.ends_at, date: endWall.date, time: endWall.time } : null,
    },
  };
}

/**
 * The instant an endpoint stands for: the one it was loaded with, while the fields
 * still read as they did.
 *
 * When the clocks go back, a wall time inside the repeated hour names two instants and
 * `wallToInstant()` answers the later one, always. Re-deriving an endpoint nobody
 * touched therefore moves it: a start on the first pass lands after an end on the
 * second, so a save that changed nothing is refused, and an event lying wholly on the
 * first pass moves an hour later with nothing on screen to say so. An endpoint that was
 * actually edited has only its wall clock to go on and resolves as before, to the
 * second pass.
 */
function instantFor(form, which) {
  const was = form.pristine && form.pristine[which];
  const date = form[`${which}_date`];
  const time = form[`${which}_time`];
  if (was && was.date === date && was.time === time) return was.at;
  return wallToInstant(date, time);
}

export async function renderEventEditor({ id, date, query }) {
  const isNew = !id;
  const presetDate = query && query.get('date');
  const presetCalendar = query && query.get('calendar');

  const body = h('div', { class: 'editor' }, spinner());
  const view = screen({
    title: isNew ? t('event.new') : t('event.edit'),
    onBack: () => back(`/${state.view}`),
  }, body);

  let form;
  if (isNew) {
    form = blankForm(presetDate, presetCalendar);
  } else {
    try {
      const data = await api.event(id, date);
      form = formFromOccurrence(normalizeOccurrence(data.occurrence), data.recurrence, data.my_reminders);
    } catch (err) {
      clear(body);
      body.appendChild(errorBox(err));
      return view;
    }
  }

  if (!form.calendar_id) {
    clear(body);
    body.appendChild(h('p', { class: 'empty' }, t('calendar.new')));
    body.appendChild(button(t('calendar.new'), { onclick: () => go('/calendars') }));
    return view;
  }

  const errors = h('div', { class: 'form-errors' });
  let saving = false;
  let saveBtn = null;

  const paint = () => {
    clear(body);
    body.appendChild(errors);
    body.appendChild(basicsSection(form, paint));
    body.appendChild(whenSection(form, paint));
    body.appendChild(peopleSection(form, paint));
    body.appendChild(detailsSection(form));
    body.appendChild(repeatSection(form, paint, isNew));
    body.appendChild(remindersSection(form, paint));
    saveBtn = button(t('action.save'), { onclick: () => save(), disabled: saving });
    body.appendChild(h('div', { class: 'editor-actions' },
      button(t('action.cancel'), { variant: 'quiet', onclick: () => back(`/${state.view}`) }),
      saveBtn));
    if (!isNew) {
      body.appendChild(h('div', { class: 'editor-danger' },
        button(t('action.delete'), {
          variant: 'danger', iconName: 'trash', wide: true,
          onclick: () => deleteOccurrence({
            event_id: form.id,
            occurrence_date: form.occurrence_date,
            title: form.title,
          }, form.is_series),
        })));
    }
  };

  const validate = () => {
    if (!form.title.trim()) return t('event.titleRequired');
    if (form.all_day) {
      if (form.end_date < form.start_date) return t('event.endBeforeStart');
    } else {
      const s = instantFor(form, 'start');
      const e = instantFor(form, 'end');
      if (e < s) return t('event.endBeforeStart');
    }
    return null;
  };

  const buildBody = () => {
    const out = {
      title: form.title.trim(),
      all_day: form.all_day,
      location: form.location.trim(),
      url: form.url.trim(),
      notes: form.notes,
      label_id: form.label_id,
      participants: form.participants,
    };
    if (form.all_day) {
      out.start_date = form.start_date;
      out.end_date = form.end_date;
    } else {
      out.starts_at = instantFor(form, 'start');
      out.ends_at = instantFor(form, 'end');
    }
    return out;
  };

  const save = async () => {
    if (saving) return;
    const problem = validate();
    formError(errors, problem);
    if (problem) return;
    saving = true;
    if (saveBtn) saveBtn.disabled = true;
    try {
      if (isNew) {
        const payload = buildBody();
        payload.calendar_id = form.calendar_id;
        if (form.recurrence) payload.recurrence = apiRecurrence(form);
        if (form.reminders.length) payload.reminders = form.reminders.map((r) => apiReminder(r, form.all_day));
        await api.createEvent(payload);
      } else {
        let scope = null;
        if (form.is_series) {
          scope = await chooseDialog(t('scope.editTitle'), SCOPE_OPTIONS());
          if (!scope) { saving = false; if (saveBtn) saveBtn.disabled = false; return; }
        }
        const payload = buildBody();
        // Recurrence only travels with a whole-series edit; 'this' and 'upcoming'
        // are about occurrences, and the server owns the split. A one-off sends none
        // at all: the API refuses to add a repeat to an existing event or remove one,
        // and this used to send `null` for both, which was accepted and then dropped.
        if (form.recurrence && scope === 'all') payload.recurrence = apiRecurrence(form);
        const saved = await api.updateEvent(form.id, payload, scope ? { scope, date: form.occurrence_date } : {});
        // Reminders are the caller's own and have their own endpoint. They are filed
        // against the event the edit answered with, not the one it was addressed to:
        // editing a single occurrence of a series leaves a standalone copy behind, and
        // that copy is what owns the reminders for that occurrence from then on. Sending
        // them to form.id — the series, on the first edit of an occurrence — changed the
        // reminder on every lesson instead of the one on screen, and left the one the
        // family had just been shown unchanged.
        const target = (saved && saved.event && saved.event.id) || form.id;
        await api.putReminders(target, form.reminders.map((r) => apiReminder(r, form.all_day)));
      }
      invalidateRange();
      go(`/${state.view}?d=${form.start_date}`);
    } catch (err) {
      saving = false;
      if (saveBtn) saveBtn.disabled = false;
      formError(errors, errorMessage(err));
    }
  };

  paint();
  return view;
}

function apiRecurrence(form) {
  const r = form.recurrence;
  const out = { freq: r.freq, interval: Math.max(1, Number(r.interval) || 1) };
  if (r.freq === 'weekly') {
    out.by_weekday = r.by_weekday && r.by_weekday.length ? r.by_weekday.slice().sort() : [weekdayOf(form.start_date)];
  }
  if (r.freq === 'monthly') {
    if (r.month_last_day) out.month_last_day = true;
    else if (r.week_ordinal) {
      out.week_ordinal = r.week_ordinal;
      out.by_weekday = [weekdayOf(form.start_date)];
    } else {
      out.by_monthday = r.by_monthday || parseDate(form.start_date).d;
    }
  }
  if (r.until) out.until = r.until;
  return out;
}

function apiReminder(r, allDay) {
  return allDay
    ? { days_before: Number(r.days_before || 0), at_time_local: r.at_time_local || '09:00' }
    : { offset_minutes: Number(r.offset_minutes || 0) };
}

// -- editor sections --------------------------------------------------------

function basicsSection(form, paint) {
  const titleInput = input({
    value: form.title,
    placeholder: t('event.title'),
    autocapitalize: 'sentences',
    enterkeyhint: 'done',
    oninput: (e) => { form.title = e.target.value; },
  });

  const calSelect = select(
    state.calendars.map((c) => ({ value: c.id, label: c.name })),
    {
      value: form.calendar_id,
      onchange: (e) => {
        form.calendar_id = Number(e.target.value);
        const labels = labelsOf(form.calendar_id);
        form.label_id = labels.length ? labels[0].id : null;
        const ids = new Set(membersOf(form.calendar_id).map((m) => m.user_id));
        form.participants = form.participants.filter((p) => ids.has(p));
        paint();
      },
    });

  return section(null,
    field(t('event.title'), titleInput),
    field(t('event.calendar'), calSelect),
    labelField(form, paint));
}

function labelField(form, paint) {
  const labels = labelsOf(form.calendar_id);
  const selected = labels.find((l) => l.id === form.label_id) || null;
  const row = h('div', { class: 'label-picker' },
    ...labels.map((l) => {
      const hex = normalizeHex(l.color);
      const active = l.id === form.label_id;
      return h('button', {
        class: ['label-swatch', active ? 'is-active' : ''].filter(Boolean).join(' '),
        type: 'button',
        'aria-label': l.name,
        title: l.name,
        'aria-pressed': String(active),
        style: { '--c': hex, '--c-on': readableOn(hex) },
        onclick: () => { form.label_id = l.id; paint(); },
      }, active ? icon('check') : null);
    }));
  return h('div', { class: 'field' },
    h('span', { class: 'field-label' }, t('event.label')),
    row,
    h('p', { class: 'field-hint' }, selected ? selected.name : ''));
}

function whenSection(form, paint) {
  const allDayRow = toggleRow(t('event.allDay'), form.all_day, (checked) => {
    form.all_day = checked;
    form.reminders = [];
    paint();
  });

  const startDate = input({
    type: 'date', value: form.start_date,
    onchange: (e) => {
      const v = e.target.value || form.start_date;
      const shift = form.end_date < v;
      form.start_date = v;
      if (shift) form.end_date = v;
      paint();
    },
  });
  const endDate = input({
    type: 'date', value: form.end_date,
    onchange: (e) => { form.end_date = e.target.value || form.end_date; paint(); },
  });

  if (form.all_day) {
    return section(null, allDayRow, field(t('event.start'), startDate), field(t('event.end'), endDate));
  }

  const startTime = input({
    type: 'time', value: form.start_time,
    onchange: (e) => { form.start_time = e.target.value || form.start_time; },
  });
  const endTime = input({
    type: 'time', value: form.end_time,
    onchange: (e) => { form.end_time = e.target.value || form.end_time; },
  });

  // Two controls under one word, and the word cannot say which of them is the date and
  // which is the time — so each names itself. The name begins with the word that is
  // written above it, so what is heard still starts with what is read. It is an
  // aria-label rather than a <label for>, which is what the all-day rows above use:
  // a label element can only name one control, and there is no second word on screen
  // for it to name the other with.
  const pair = (labelText, dateInput, timeInput) => {
    dateInput.setAttribute('aria-label', t('event.dateOf', { field: labelText }));
    timeInput.setAttribute('aria-label', t('event.timeOf', { field: labelText }));
    return h('div', { class: 'field' },
      h('span', { class: 'field-label' }, labelText),
      h('div', { class: 'field-pair' }, dateInput, timeInput));
  };

  return section(null, allDayRow,
    pair(t('event.start'), startDate, startTime),
    pair(t('event.end'), endDate, endTime));
}

function peopleSection(form) {
  const members = membersOf(form.calendar_id);
  return section(t('event.participants'),
    members.length
      ? memberSelector(members, form.participants, (ids) => { form.participants = ids; })
      : h('p', { class: 'field-hint' }, t('calendar.members')));
}

function detailsSection(form) {
  return section(null,
    field(t('event.location'), input({
      value: form.location, autocapitalize: 'sentences',
      oninput: (e) => { form.location = e.target.value; },
    })),
    field(t('event.url'), input({
      value: form.url, type: 'url', inputmode: 'url', autocapitalize: 'off',
      oninput: (e) => { form.url = e.target.value; },
    })),
    field(t('event.notes'), textarea({
      value: form.notes,
      oninput: (e) => { form.notes = e.target.value; },
    })));
}

// repeatSection edits the repeat pattern. Whether an event repeats at all is settled when
// it is created: the API refuses to add a repeat to an existing event or to take one away,
// because either would have to move the family's reminders and decide the fate of the
// occurrences somebody has already edited by hand. So for an existing event this offers
// the pattern and nothing else, and says why.
function repeatSection(form, paint, isNew) {
  if (!isNew && !form.recurrence) {
    return section(t('recur.repeat'), h('p', { class: 'field-hint' }, t('recur.lockedOneOff')));
  }
  const freq = form.recurrence ? form.recurrence.freq : 'none';
  const freqSelect = select([
    ...(isNew ? [{ value: 'none', label: t('recur.none') }] : []),
    { value: 'daily', label: t('recur.daily') },
    { value: 'weekly', label: t('recur.weekly') },
    { value: 'monthly', label: t('recur.monthly') },
    { value: 'yearly', label: t('recur.yearly') },
  ], {
    value: freq,
    onchange: (e) => {
      const v = e.target.value;
      if (v === 'none') form.recurrence = null;
      else {
        form.recurrence = Object.assign({
          interval: 1, by_weekday: [], by_monthday: null, week_ordinal: null,
          month_last_day: false, until: null,
        }, form.recurrence || {}, { freq: v });
        if (v === 'weekly' && !form.recurrence.by_weekday.length) {
          form.recurrence.by_weekday = [weekdayOf(form.start_date)];
        }
      }
      paint();
    },
  });

  const parts = [field(t('recur.repeat'), freqSelect, isNew ? null : t('recur.lockedSeries'))];

  if (form.recurrence) {
    const r = form.recurrence;
    const unitKey = { daily: 'days', weekly: 'weeks', monthly: 'months', yearly: 'years' }[r.freq];
    parts.push(h('div', { class: 'field' },
      h('span', { class: 'field-label' }, t('recur.interval')),
      h('div', { class: 'interval-row' },
        input({
          type: 'number', min: 1, max: 99, value: String(r.interval || 1), inputmode: 'numeric',
          class: 'input input-narrow',
          onchange: (e) => { r.interval = Math.max(1, Number(e.target.value) || 1); },
        }),
        h('span', { class: 'interval-unit' }, t(`recur.interval.${unitKey}`)))));

    if (r.freq === 'weekly') {
      parts.push(h('div', { class: 'field' },
        h('span', { class: 'field-label' }, t('recur.weekdays')),
        h('div', { class: 'weekday-picker' }, ...weekdayOrder(weekStart()).map((dow) => {
          const on = r.by_weekday.includes(dow);
          return h('button', {
            class: ['weekday-btn', on ? 'is-active' : ''].filter(Boolean).join(' '),
            type: 'button',
            'aria-pressed': String(on),
            'aria-label': weekdayName(dow),
            onclick: () => {
              if (on) r.by_weekday = r.by_weekday.filter((d) => d !== dow);
              else r.by_weekday = r.by_weekday.concat([dow]).sort();
              paint();
            },
          }, weekdayName(dow, 'narrow'));
        }))));
    }

    if (r.freq === 'monthly') {
      const day = parseDate(form.start_date).d;
      const dow = weekdayOf(form.start_date);
      const ordinal = Math.ceil(day / 7);
      const mode = r.month_last_day ? 'last' : (r.week_ordinal ? 'weekday' : 'day');
      const options = [
        { value: 'day', label: t('recur.monthlyMode.day', { day }) },
        { value: 'weekday', label: t('recur.monthlyMode.weekday', { ordinal: t(`recur.ordinal.${ordinal}`), weekday: weekdayName(dow) }) },
        { value: 'last', label: t('recur.monthlyMode.lastDay') },
      ];
      if (day > daysInMonth(parseDate(form.start_date).y, parseDate(form.start_date).m) - 7) {
        options.splice(2, 0, {
          value: 'lastWeekday',
          label: t('recur.monthlyMode.weekday', { ordinal: t('recur.ordinal.-1'), weekday: weekdayName(dow) }),
        });
      }
      parts.push(field(t('recur.monthly'), select(options, {
        value: mode === 'weekday' && r.week_ordinal === -1 ? 'lastWeekday' : mode,
        onchange: (e) => {
          const v = e.target.value;
          r.month_last_day = v === 'last';
          r.week_ordinal = v === 'weekday' ? ordinal : (v === 'lastWeekday' ? -1 : null);
          r.by_monthday = v === 'day' ? day : null;
          paint();
        },
      })));
    }

    const forever = !r.until;
    parts.push(toggleRow(t('recur.forever'), forever, (checked) => {
      r.until = checked ? null : addDays(form.start_date, 365);
      paint();
    }));
    if (!forever) {
      parts.push(field(t('recur.until'), input({
        type: 'date', value: r.until,
        onchange: (e) => { r.until = e.target.value || null; paint(); },
      })));
    }
  }

  return section(t('recur.repeat'), ...parts);
}

function remindersSection(form, paint) {
  const used = new Set(form.reminders.map((r) => reminderKey(r, form.all_day)));
  const presets = form.all_day
    ? ALLDAY_PRESETS.map((p) => ({ value: reminderKey(p, true), reminder: p }))
    : TIMED_PRESETS.map((m) => ({ value: reminderKey({ offset_minutes: m }, false), reminder: { offset_minutes: m } }));
  const available = presets.filter((p) => !used.has(p.value));

  const list = form.reminders.length
    ? h('ul', { class: 'reminder-list' }, ...form.reminders.map((r, i) =>
      h('li', { class: 'reminder-item' },
        h('span', null, reminderLabel(r, form.all_day)),
        h('button', {
          class: 'icon-btn icon-btn-quiet', type: 'button', 'aria-label': t('action.delete'),
          onclick: () => { form.reminders.splice(i, 1); paint(); },
        }, icon('close')))))
    : h('p', { class: 'field-hint' }, t('reminder.none'));

  const adder = available.length
    ? select(
      [{ value: '', label: t('reminder.add') }].concat(available.map((p) => ({
        value: p.value, label: reminderLabel(p.reminder, form.all_day),
      }))),
      {
        value: '',
        onchange: (e) => {
          const pick = available.find((p) => p.value === e.target.value);
          if (!pick) return;
          form.reminders.push(Object.assign({}, pick.reminder));
          paint();
        },
      })
    : null;

  return section(t('reminder.title'), list, adder, h('p', { class: 'field-hint' }, t('reminder.hint')));
}
