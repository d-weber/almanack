// The day a search result links to.
//
// A search answers with events, not occurrences, so every row has to be told which day it
// stands for before it can link anywhere. The server now says: it expands the rule anyway
// to work out when the event happens next, and `occurrence_date` is the day the row opens
// on — the next occurrence while there is one, and the series' final occurrence once
// there is not. The screen used to derive it instead, from a chain of three guesses, and
// both bugs below are guesses that were wrong in the one case that reached them.
//
// It fails hard rather than looking slightly wrong. The date becomes the route and the
// `?date=` the detail screen asks the API for, and a date the series does not land on is
// answered with a 404 — so tapping a search result showed "Not found." and the event
// looked deleted. Both tests pin that: the API really does refuse the day the old chain
// produced, so there was nothing between it and the family seeing an error.
//
// Two ways to reach it, and they are genuinely different faults:
//
//   - #64, a timezone one. The chain's last leg read the day off the front of an RFC 3339
//     instant, which is the UTC day and not the family's for the hour either side of
//     midnight. Only timed events have an instant, so only timed events could reach it.
//   - #69, an anchor one. The chain's middle leg used the event's own start date, on the
//     assumption that a series starts on one of its own occurrences. DTStart is only the
//     interval anchor: a weekly rule can exclude the weekday it starts on, and the editor
//     will build one. This reaches **all-day** events, where there is no instant and no
//     timezone anywhere, which is why no amount of fixing #64 could have touched it.
//
// These are browser tests because the date layer has no other harness — the frontend takes
// no dependencies (CONVENTIONS §1), so there is no JavaScript unit runner to put them in.
// The fixtures are created through the API rather than the editor for the same reason
// e2e/dst-fallback.spec.js does it: the editor cannot express a series that finished six
// months ago without a great deal of clicking that is not what is under test. Each is
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
// Tuesdays from 6 January until the 20th: the 6th, the 13th and the 20th. The last one
// is the day the row opens on, because a finished series links to when it last ran.
const LAST_DAY = '2026-01-20';

// The all-day series from #69. Monday 5 January anchors a rule that happens on Tuesdays,
// so the anchor is not an occurrence of the rule it anchors — the occurrences are the
// 6th, 13th, 20th and 27th. No instant is involved anywhere in this one.
const ALL_DAY_TITLE = 'Atelier poterie';
const ANCHOR_DAY = '2026-01-05'; // Monday: the start date, and not an occurrence
const ALL_DAY_LAST = '2026-01-27'; // the last Tuesday the series ran
const TUESDAY = 2; // by_weekday is 0=Sunday..6=Saturday

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

/** Creates an event through the API and returns its id. */
async function createEvent(page, fields) {
  const me = await (await page.request.get('/api/v1/me')).json();
  const calendar = me.calendars[0];
  const response = await page.request.post('/api/v1/events', {
    headers: HEADERS,
    data: {
      calendar_id: calendar.id,
      label_id: calendar.labels[0].id,
      participants: [],
      ...fields,
    },
  });
  expect(response.status(), await response.text()).toBe(201);
  return (await response.json()).event.id;
}

/** A weekly timed series that ran for three weeks in January. */
function createFinishedSeries(page) {
  return createEvent(page, {
    title: TITLE,
    all_day: false,
    starts_at: STARTS_AT,
    ends_at: ENDS_AT,
    recurrence: { freq: 'weekly', interval: 1, until: UNTIL },
  });
}

/** A weekly all-day series anchored on a day its own rule excludes. */
function createFinishedAllDaySeries(page) {
  return createEvent(page, {
    title: ALL_DAY_TITLE,
    all_day: true,
    start_date: ANCHOR_DAY,
    end_date: ANCHOR_DAY,
    recurrence: {
      freq: 'weekly', interval: 1, by_weekday: [TUESDAY], until: ALL_DAY_LAST,
    },
  });
}

/**
 * Searches for a title in the UI and taps the row, returning once the detail screen has
 * been routed to. The row is a button carrying the title, which is how the results list
 * renders every hit.
 */
async function searchAndOpen(page, title) {
  await page.goto('/#/search');
  await page.getByRole('searchbox').fill(title);
  const row = page.getByRole('button', { name: new RegExp(title) });
  await expect(row).toBeVisible();
  await row.click();
}

/**
 * The preconditions both bugs needed, pinned rather than assumed.
 *
 * The series is over, so the server offers no next occurrence and the row cannot simply
 * copy one; and the day the old chain derived instead is one the API refuses outright.
 * Without these the tests below would go on passing over a fixture that had quietly
 * stopped being a finished series, asserting nothing.
 */
async function assertTheSeriesIsOverAndTheOldDayIsRefused(page, id, title, oldDay) {
  const found = await (await page.request.get(`/api/v1/search?q=${encodeURIComponent(title)}`)).json();
  const mine = found.results.find((r) => r.event.id === id);
  expect(mine, 'the search must find the series it is about').toBeTruthy();
  expect(mine.next_occurrence, 'a series that has ended has no next occurrence').toBeNull();

  const refused = await page.request.get(`/api/v1/events/${id}?date=${oldDay}`);
  expect(refused.status(), `${oldDay} must not be an occurrence of this series`).toBe(404);
}

test('a timed series that has ended opens on a day it happened, not the UTC day', async ({ page }) => {
  await page.goto('/');
  await assertTheDaysDiffer(page);

  const id = await createFinishedSeries(page);

  try {
    await assertTheSeriesIsOverAndTheOldDayIsRefused(page, id, TITLE, UTC_DAY);

    await searchAndOpen(page, TITLE);

    // The route carries the date, so this is the assertion the whole test is for. It read
    // 2026-01-05 before #64 — the UTC day — and the screen behind it said "Not found."
    // It is the 20th rather than the anchor on the 6th because the server answers now,
    // and what it answers for a finished series is the last day it ran.
    await expect(page).toHaveURL(new RegExp(`#/event/${id}/${LAST_DAY}$`));
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

test('an all-day series whose rule excludes its own start date still opens', async ({ page }) => {
  await page.goto('/');

  const id = await createFinishedAllDaySeries(page);

  try {
    // The start date is the day the old chain linked to, and the API refuses it: the
    // anchor is a Monday and the rule says Tuesday. Nothing here is a timezone question,
    // which is what makes this a different bug from the one above rather than a repeat.
    await assertTheSeriesIsOverAndTheOldDayIsRefused(page, id, ALL_DAY_TITLE, ANCHOR_DAY);

    await searchAndOpen(page, ALL_DAY_TITLE);

    // It read 2026-01-05 before — the anchor — and the screen said "Not found." about an
    // event that was still there.
    await expect(page).toHaveURL(new RegExp(`#/event/${id}/${ALL_DAY_LAST}$`));
    await expect(page.locator('.detail-title')).toHaveText(ALL_DAY_TITLE);
  } finally {
    // scope=all: see the test above. A series left behind here is found by the next run
    // and by three other specs that count what the seeded family has.
    const gone = await page.request.delete(`/api/v1/events/${id}?scope=all`, { headers: HEADERS });
    expect(gone.status(), await gone.text()).toBe(204);
  }
});
