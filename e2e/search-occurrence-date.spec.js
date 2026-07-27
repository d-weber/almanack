// The day a search result links to.
//
// A search answers with events, not occurrences, so the results screen has to work out
// which day each row stands for before it can link anywhere. Three sources, in order:
// the next occurrence the server computed, the event's own start date, and — for a timed
// event whose series has already ended, which is the only way to reach it — the day its
// start instant falls on. That last one used to be the first ten characters of the RFC
// 3339 text, which is the UTC day and not the family's, and the two are different days
// for the hour or so either side of midnight.
//
// It fails hard rather than looking slightly wrong. The date becomes the route and the
// `?date=` the detail screen asks the API for, and a date the series does not land on is
// answered with a 404 — so tapping a search result showed "Not found." and the event
// looked deleted. That is asserted below: the server really does refuse the UTC day, so
// there is nothing between the client's arithmetic and the family seeing an error.
//
// This is a browser test because the date layer has no other harness — the frontend takes
// no dependencies (CONVENTIONS §1), so there is no JavaScript unit runner to put it in.
// The fixture is created through the API rather than the editor for the same reason
// e2e/dst-fallback.spec.js does it: the editor cannot express a series that finished six
// months ago without a great deal of clicking that is not what is under test. It is
// deleted again in a finally, so the seeded family is left as it was found.

import { test, expect } from '@playwright/test';
import { HEADERS } from './fixtures.js';

// Paris is an hour ahead of UTC in January, so the family's 6 January begins at 23:00Z
// on the 5th and half past midnight is stored on the day before. Three Tuesdays, the
// last of them in January 2026, which is long over and will stay that way.
const TITLE = 'Night shift handover';
const STARTS_AT = '2026-01-05T23:30:00Z'; // 00:30 Paris, Tuesday 6 January
const ENDS_AT = '2026-01-06T00:30:00Z'; // 01:30 Paris, the same morning
const FAMILY_DAY = '2026-01-06'; // the day the family had it, and the series' anchor
const UTC_DAY = '2026-01-05'; // the day the instant's text begins with
const UNTIL = '2026-01-20';

/**
 * The two days really are different in this browser's tzdata.
 *
 * Without this the test would go on passing on a browser whose Paris sat on UTC, having
 * quietly stopped testing anything. If it ever fails, the zone has changed its winter
 * offset and the constants above need a date that still straddles midnight — not a
 * weaker assertion.
 */
async function assertTheDaysDiffer(page) {
  const day = await page.evaluate((instant) => new Intl.DateTimeFormat('en-CA', {
    timeZone: 'Europe/Paris', year: 'numeric', month: '2-digit', day: '2-digit',
  }).format(new Date(instant)), STARTS_AT);
  expect(day, `${STARTS_AT} must be ${FAMILY_DAY} in Paris while its text says ${UTC_DAY}`)
    .toBe(FAMILY_DAY);
}

/** A weekly timed series that ran for three weeks in January, straight through the API. */
async function createFinishedSeries(page) {
  const me = await (await page.request.get('/api/v1/me')).json();
  const calendar = me.calendars[0];
  const response = await page.request.post('/api/v1/events', {
    headers: HEADERS,
    data: {
      calendar_id: calendar.id,
      label_id: calendar.labels[0].id,
      title: TITLE,
      all_day: false,
      starts_at: STARTS_AT,
      ends_at: ENDS_AT,
      participants: [],
      recurrence: { freq: 'weekly', interval: 1, until: UNTIL },
    },
  });
  expect(response.status(), await response.text()).toBe(201);
  return (await response.json()).event.id;
}

test('a search result for a series that has ended opens on a day it happened', async ({ page }) => {
  await page.goto('/');
  await assertTheDaysDiffer(page);

  const id = await createFinishedSeries(page);

  try {
    // The two things the bug needed, pinned rather than assumed: the series is over, so
    // the server offers no next occurrence and the client has to derive the day itself…
    const found = await (await page.request.get(`/api/v1/search?q=${encodeURIComponent(TITLE)}`)).json();
    const mine = found.results.find((r) => r.event.id === id);
    expect(mine, 'the search must find the series it is about').toBeTruthy();
    expect(mine.next_occurrence, 'a series that has ended has no next occurrence').toBeNull();

    // …and the day it used to derive is one the API refuses outright.
    const wrongDay = await page.request.get(`/api/v1/events/${id}?date=${UTC_DAY}`);
    expect(wrongDay.status(), `${UTC_DAY} must not be an occurrence of this series`).toBe(404);

    await page.goto('/#/search');
    await page.getByRole('searchbox').fill(TITLE);
    const row = page.getByRole('button', { name: new RegExp(TITLE) });
    await expect(row).toBeVisible();
    await row.click();

    // The route carries the date, so this is the assertion the whole test is for. It
    // read 2026-01-05 before, and the screen behind it said "Not found."
    await expect(page).toHaveURL(new RegExp(`#/event/${id}/${FAMILY_DAY}$`));
    await expect(page.locator('.detail-title')).toHaveText(TITLE);
  } finally {
    // scope=all, because deleting an occurrence of a series is a different request from
    // deleting the series and the API will not guess which was meant: without it this
    // leaves the whole series behind, and a run that has been round twice then finds
    // three rows where it expected one. See e2e/smoke.spec.js on why a fixture that
    // outlives its test costs somebody else the afternoon.
    const gone = await page.request.delete(`/api/v1/events/${id}?scope=all`, { headers: HEADERS });
    expect(gone.status(), await gone.text()).toBe(204);
  }
});
