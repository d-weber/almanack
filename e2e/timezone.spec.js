// The device-timezone tests, run by the `chromium-lisbon` project only — the config
// keeps them out of the Paris project, where the Lisbon assertion below could never
// hold and was failing for a reason that had nothing to do with the application.
//
// This is the bug the whole date layer is built to avoid: a family member travelling
// (or a laptop set to the wrong zone) must still see the Paris dentist appointment at
// its Paris time. Intl defaults to the *device* timezone, so any date rendered
// without an explicit timeZone option silently shifts by an hour here — and an
// all-day event shifts by a whole day.
//
// These start already signed in, from the session auth.setup.js saved.

import { test, expect } from '@playwright/test';

const HOLIDAY = 'Seaside holiday';

/**
 * Calendar arithmetic on 'YYYY-MM-DD', done the way web/js/dates.js does it: a UTC
 * midnight used as an integer calendar and never as a moment, so no zone can reach it.
 */
function addDays(iso, n) {
  const [y, m, d] = iso.split('-').map(Number);
  return new Date(Date.UTC(y, m - 1, d + n)).toISOString().slice(0, 10);
}

/** The first day of the week holding `iso`, for a week that begins on `weekStart`. */
function startOfWeek(iso, weekStart) {
  const [y, m, d] = iso.split('-').map(Number);
  const weekday = new Date(Date.UTC(y, m - 1, d)).getUTCDay();
  return addDays(iso, -((weekday - weekStart + 7) % 7));
}

/**
 * The days the family's seaside holiday covers, as the server states them.
 *
 * Asked of the API rather than recomputed from the seed's rule, because where the demo
 * puts the holiday is the seed's to decide and it has moved once already: #72 pinned it
 * to the second Saturday of the seeded month so that the whole span lands on the screen
 * the app opens on. What is asked below is only whether this browser agrees with the
 * server about which days those are, so the server is where to get them from.
 *
 * Search rather than a range read, because search takes no window: the seeded month is
 * whichever month `make seed` was run in, and a window would have to be guessed from a
 * clock — in this file of all files, the wrong instrument for finding out what day it is.
 * auth.setup.js asks the same endpoint the same way, for the same reason.
 */
async function seasideHoliday(page) {
  const answer = await page.request.get(`/api/v1/search?q=${encodeURIComponent(HOLIDAY)}`);
  expect(answer.status(), await answer.text()).toBe(200);

  const found = (await answer.json()).results.filter((hit) => hit.event.title === HOLIDAY);
  expect(found, `\`make seed\` puts one "${HOLIDAY}" on the month it seeds`).toHaveLength(1);
  expect(found[0].event.all_day, 'the multi-day all-day case is what this test is for').toBe(true);
  return found[0].event;
}

test('events render in the family timezone, not the device one', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByRole('button', { name: /Today/i })).toBeVisible();

  // Confirm the browser really is in Lisbon (one hour behind Paris), or this test
  // proves nothing.
  const deviceZone = await page.evaluate(() => Intl.DateTimeFormat().resolvedOptions().timeZone);
  expect(deviceZone).toBe('Europe/Lisbon');

  // The seeded dentist appointment is at 16:30 Paris time. A device-timezone bug
  // would render it as 15:30.
  //
  // The first assertion is scoped to the detail screen the click opened. 16:30 matches
  // more than once — the month chip behind it draws the same string — so taking the
  // first match was satisfied whether or not the screen under test had rendered at all,
  // which is the fault #78 was filed about, one test further down this file. The second
  // assertion is deliberately not scoped: 15:30 must appear nowhere on the page, and a
  // page is exactly the breadth a device-timezone regression could surface anywhere in.
  const card = page.getByText("Leo's dentist").first();
  await card.click();
  await expect(page.locator('.event-detail')).toContainText('16:30');
  await expect(page.getByText('15:30')).toHaveCount(0);
});

test('an all-day event does not slip to the previous day', async ({ page }) => {
  // "Seaside holiday" is seeded as a multi-day all-day event. Stored as a midnight
  // instant it would start a day early west of Paris; stored as a date it cannot.
  const holiday = await seasideHoliday(page);

  // The day sheet, which is the one screen that names a single day and lists what is on
  // it. Seven of them, across the week the holiday begins in.
  //
  // This used to read the agenda, and the agenda cannot answer the question: it renders
  // from today forward, so for the eleven-odd days a month that follow the span the
  // holiday is not on that screen at all, and the assertion passed only by finding the
  // month grid's copy of the title before the agenda replaced it (#78). Nor can the month
  // grid — it draws a span as one bar across a week row, so a day early is a grid column
  // rather than a day anything here could name.
  //
  // It read the week screen until that view was removed, for exactly this property. The
  // day sheet has it too and is reachable by URL, which is all this needs.
  //
  // The week start is a per-user display preference, so it is read rather than assumed:
  // the settings screen offers it, and unknown-tz.spec.js changes it and puts it back.
  const weekStart = (await (await page.request.get('/api/v1/me')).json()).user.week_start;
  const first = startOfWeek(holiday.start_date, weekStart);
  expect(
    first < holiday.start_date,
    `the week beginning ${first} must hold the day before ${holiday.start_date}, or this `
      + 'asks only where the holiday is and never whether it moved',
  ).toBe(true);

  for (let i = 0; i < 7; i++) {
    const day = addDays(first, i);
    const onIt = day >= holiday.start_date && day <= holiday.end_date;
    await page.goto(`/#/day/${day}`);
    const sheet = page.locator('.day-sheet');
    await expect(sheet).toBeVisible({ timeout: 15_000 });
    // Scoped to the sheet, since the month grid behind it draws the same title across
    // the whole span and an unscoped assertion would be satisfied by that — which is
    // the shape of the slip this test is named for.
    await expect(
      sheet.getByText(HOLIDAY, { exact: true }),
      `"${HOLIDAY}" on ${day}`,
    ).toHaveCount(onIt ? 1 : 0);
  }
});
