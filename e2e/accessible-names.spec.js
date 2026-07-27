// What the calendar is called when it is heard rather than read.
//
// Two controls in this app carried no name at all: the second week of a multi-day bar,
// which a screen reader announced as "button", and the four date and time boxes of a
// timed event, which it announced as their type and nothing else. Neither is visible in
// a screenshot and neither can fail a Go test — the accessible name is computed by the
// browser from the DOM, so a real browser is the only place it exists.
//
// These assert the name, not the markup that produces it: `getByRole` and `getByLabel`
// resolve exactly what assistive technology would, so they keep holding if the way the
// name is attached ever changes.

import { test, expect } from '@playwright/test';

const HEADERS = { 'X-Requested-With': 'almanack' };

const HOLIDAY = 'A week by the sea';

/**
 * A seven-day all-day event starting on a Wednesday, in the quiet middle of a month
 * two months out.
 *
 * Wednesday is the point: seven days from one crosses a week boundary whether the
 * reader's week begins on Monday or on Sunday, and lands in exactly two rows either
 * way. The seeded family's own seaside holiday runs from today + 10, so which day of
 * the week it starts on — and whether it is split at all — depends on the day the suite
 * happens to run, and a test that only fails on Fridays is worse than none.
 */
function seasideWeek() {
  const now = new Date();
  const start = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() + 2, 8));
  while (start.getUTCDay() !== 3) start.setUTCDate(start.getUTCDate() + 1);
  const end = new Date(start);
  end.setUTCDate(end.getUTCDate() + 6);
  const iso = (d) => d.toISOString().slice(0, 10);
  return { start: iso(start), end: iso(end) };
}

async function createAllDayEvent(page, { title, start_date, end_date }) {
  const me = await (await page.request.get('/api/v1/me')).json();
  const calendar = me.calendars[0];
  const response = await page.request.post('/api/v1/events', {
    headers: HEADERS,
    data: {
      calendar_id: calendar.id,
      label_id: calendar.labels[0].id,
      title,
      all_day: true,
      start_date,
      end_date,
      participants: [],
    },
  });
  expect(response.status(), await response.text()).toBe(201);
  return (await response.json()).event.id;
}

async function deleteEvent(page, id) {
  await page.request.delete(`/api/v1/events/${id}`, { headers: HEADERS });
}

test('a bar that runs into the next week is still named there', async ({ page }) => {
  await page.goto('/');
  const { start, end } = seasideWeek();
  const id = await createAllDayEvent(page, { title: HOLIDAY, start_date: start, end_date: end });

  try {
    await page.goto(`/#/month?d=${start}`);

    // One event, two buttons: the week it starts in and the week it finishes in. Both
    // must answer to the name of the holiday — the second one used to answer to nothing,
    // which is the whole of this test.
    const segments = page.getByRole('button', { name: HOLIDAY });
    await expect(segments).toHaveCount(2);
    await expect(segments.nth(1)).toHaveAccessibleName(HOLIDAY);

    // And it is named, not relabelled: the title is still drawn once, in the week the
    // holiday begins. A 20px bar has room for it once.
    await expect(segments.first()).toHaveText(HOLIDAY);
    await expect(segments.nth(1)).toHaveText('');
  } finally {
    await deleteEvent(page, id);
  }
});

test('the timed editor names each of its date and time boxes', async ({ page }) => {
  await page.goto('/#/event/new');

  // A new event is timed, so the start and end each become a date box and a time box
  // under one word. That word cannot say which is which, and neither box said anything
  // at all: four controls announced as "date" and "time", in a form with two of each.
  for (const [name, type] of [
    ['Starts, date', 'date'],
    ['Starts, time', 'time'],
    ['Ends, date', 'date'],
    ['Ends, time', 'time'],
  ]) {
    const box = page.getByLabel(name);
    await expect(box).toBeVisible();
    await expect(box).toHaveAttribute('type', type);
  }
});

test('the all-day editor keeps the labels it already had', async ({ page }) => {
  await page.goto('/#/event/new');
  await page.getByLabel('All day').check();

  // The all-day branch builds its rows with field(), which wires `for`/`id`, and the
  // time boxes are gone. Naming the timed branch must not have cost it that.
  await expect(page.getByLabel('Starts')).toHaveAttribute('type', 'date');
  await expect(page.getByLabel('Ends')).toHaveAttribute('type', 'date');
});
