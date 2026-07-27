// The two hours a year that are not names for moments.
//
// When Europe/Paris falls back, the wall clock runs 02:00–02:59 and then runs it again.
// A wall time inside it does not identify an instant, so the editor cannot round-trip an
// event through 'YYYY-MM-DD' + 'HH:MM' and get the same event back: `wallToInstant()`
// answers the second pass, always. Two things went wrong because of that, and both are
// asserted below against instants rather than against anything on screen — the screen
// says 02:10 either way, which is exactly what made the second one invisible. The third
// test is the other side of the fix: a time somebody actually typed must still be read
// off the clock face, or the editor would have stopped being able to change one.
//
// The fourth test is the other broken hour: the one in spring that never happens at all,
// where 02:30 means 03:30. Together with the third it pins both ambiguity rows of the
// recurrence policy table from the browser's side — the third says which pass the repeated
// hour takes, the fourth says which way the skipped hour moves. `internal/events` pins the
// same two rules from the server's side, in
// TestWallTimeInAnHourTheClocksBreakResolvesToOnePinnedInstant. That pairing is the whole
// point: the two halves of the application must not answer a broken hour differently, and
// nothing louder than a pair of tests would ever tell anyone that they had started to,
// because every candidate instant reads the same on a clock face.
//
// There is a third rule those two are subordinate to — a broken wall time may not resolve
// onto another day — and nothing here can reach it: only a missing hour that touches a date
// boundary can break it, and the seeded family is in Paris, which jumps at 02:00. `wallToInstant()`
// therefore carries a branch these tests never take. It is pinned on the server's side in
// `internal/domain`, over every minute of a year in the zones concerned, and the policy
// table in docs/architecture.md carries the row.
//
// These are browser tests because the date layer has no other test harness: the frontend
// takes no dependencies (CONVENTIONS §1), so there is no JavaScript unit runner to put
// them in.
//
// The fixtures are created through the API rather than the editor, because the editor is
// the thing under test: it cannot express "02:10, the first one" and so could not set up
// either case. They are torn down again, so the seeded family is left as it was found.

import { test, expect } from '@playwright/test';

// Paris falls back on this date at 03:00 CEST → 02:00 CET, so 01:00Z is the seam.
const DATE = '2026-10-25';
const FIRST_PASS_0210 = '2026-10-25T00:10:00Z'; // 02:10 CEST, before the seam
const FIRST_PASS_0250 = '2026-10-25T00:50:00Z'; // 02:50 CEST, before the seam
const SECOND_PASS_0210 = '2026-10-25T01:10:00Z'; // 02:10 CET, after it
const SECOND_PASS_0220 = '2026-10-25T01:20:00Z'; // 02:20 CET

// Paris springs forward on this date at 02:00 CET → 03:00 CEST, so 02:00–02:59 is skipped.
const SPRING_DATE = '2026-03-29';
const SPRING_0130 = '2026-03-29T00:30:00Z'; // 01:30 CET, the last half-hour before the gap
const SPRING_0430 = '2026-03-29T02:30:00Z'; // 04:30 CEST, safely after it
const SPRING_SEAM = '2026-03-29T01:00:00Z'; // 03:00 CEST: the first instant of the new offset
const NORMALISED_0230 = '2026-03-29T01:30:00Z'; // what 02:30 becomes: 03:30 CEST

const HEADERS = { 'X-Requested-With': 'almanack' };

/** Wall clock of some instants in Paris, as the app itself would read them. */
async function parisWall(page, instants) {
  return page.evaluate((list) => list.map((i) =>
    new Intl.DateTimeFormat('en-GB', { timeZone: 'Europe/Paris', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })
      .format(new Date(i))), instants);
}

/**
 * The hour really is ambiguous in this browser's tzdata.
 *
 * Without this the tests below would still pass on a browser whose Paris had no
 * fall-back at all — they would simply have stopped testing anything. If this ever
 * fails, the changeover has been abolished and the constants above need a date that
 * still has one, not a weaker assertion.
 */
async function assertTheHourExistsTwice(page) {
  const wall = await parisWall(page, [FIRST_PASS_0210, SECOND_PASS_0210]);
  expect(wall, `${FIRST_PASS_0210} and ${SECOND_PASS_0210} must both be 02:10 in Paris`)
    .toEqual(['02:10', '02:10']);
}

/**
 * And the spring hour really is missing: the clock goes 01:59 straight to 03:00, so no
 * instant at all reads 02:something. Same guard, same reason — a browser without the
 * changeover would turn the test below into an assertion about nothing.
 */
async function assertTheHourNeverHappens(page) {
  const wall = await parisWall(page, ['2026-03-29T00:59:00Z', SPRING_SEAM]);
  expect(wall, `the Paris clock must jump from 01:59 to 03:00 on ${SPRING_DATE}`)
    .toEqual(['01:59', '03:00']);
}

/** Create a timed event straight through the API, and hand back its id. */
async function createEvent(page, { title, starts_at, ends_at }) {
  const me = await (await page.request.get('/api/v1/me')).json();
  const calendar = me.calendars[0];
  const response = await page.request.post('/api/v1/events', {
    headers: HEADERS,
    data: {
      calendar_id: calendar.id,
      label_id: calendar.labels[0].id,
      title,
      all_day: false,
      starts_at,
      ends_at,
      participants: [],
    },
  });
  expect(response.status(), await response.text()).toBe(201);
  return (await response.json()).event.id;
}

/** What the server holds now, which is the only thing that settles any of these. */
async function storedTimes(page, id, date = DATE) {
  const body = await (await page.request.get(`/api/v1/events/${id}?date=${date}`)).json();
  return { title: body.occurrence.title, starts_at: body.occurrence.starts_at, ends_at: body.occurrence.ends_at };
}

async function deleteEvent(page, id) {
  await page.request.delete(`/api/v1/events/${id}`, { headers: HEADERS });
}

test('an event across the fall-back seam can still be saved', async ({ page }) => {
  await page.goto('/');
  await assertTheHourExistsTwice(page);

  // 02:50 CEST to 02:10 CET: forty minutes long, and its wall clock goes backwards.
  const id = await createEvent(page, {
    title: 'Across the seam',
    starts_at: FIRST_PASS_0250,
    ends_at: SECOND_PASS_0210,
  });

  try {
    await page.goto(`/#/event/${id}/${DATE}/edit`);
    const times = page.locator('input[type="time"]');
    await expect(times.first()).toHaveValue('02:50');
    await expect(times.last()).toHaveValue('02:10');

    // Change nothing at all, and save. The editor used to read both wall times back as
    // the second pass, which put the end forty minutes before the start and refused the
    // save — leaving the family with an appointment they could never edit again.
    await page.getByRole('button', { name: /^Save$/ }).click();

    await expect(page.getByText('The end must come after the start.')).toHaveCount(0);
    await expect(page).not.toHaveURL(/\/edit$/);

    // And a save that goes through must not have moved it either.
    expect(await storedTimes(page, id)).toEqual({
      title: 'Across the seam',
      starts_at: FIRST_PASS_0250,
      ends_at: SECOND_PASS_0210,
    });
  } finally {
    await deleteEvent(page, id);
  }
});

test('correcting the title of an event in the fall-back hour does not move it', async ({ page }) => {
  await page.goto('/');
  await assertTheHourExistsTwice(page);

  // Entirely inside the first pass: 02:10–02:50 CEST. Nothing about this looks wrong on
  // screen, before or after — which is why it went unnoticed.
  const id = await createEvent(page, {
    title: 'Sunrise swim',
    starts_at: FIRST_PASS_0210,
    ends_at: FIRST_PASS_0250,
  });

  try {
    await page.goto(`/#/event/${id}/${DATE}/edit`);
    await expect(page.getByLabel(/^Title$/)).toHaveValue('Sunrise swim');

    await page.getByLabel(/^Title$/).fill('Sunrise swim, corrected');
    await page.getByRole('button', { name: /^Save$/ }).click();
    await expect(page).not.toHaveURL(/\/edit$/);

    // The title is all that was touched, so the instants must be untouched. The editor
    // used to rewrite both of them to the CET pass and move the appointment an hour
    // later, silently, on a spelling fix.
    expect(await storedTimes(page, id)).toEqual({
      title: 'Sunrise swim, corrected',
      starts_at: FIRST_PASS_0210,
      ends_at: FIRST_PASS_0250,
    });
  } finally {
    await deleteEvent(page, id);
  }
});

test('a time edited inside the fall-back hour still resolves, and to the later pass', async ({ page }) => {
  await page.goto('/');
  await assertTheHourExistsTwice(page);

  // The pristine instant is only kept while the fields are untouched. Move the end and
  // the editor has nothing but a wall time to go on, so it takes the second pass — the
  // documented choice, and the one that keeps a save from being refused.
  const id = await createEvent(page, {
    title: 'Edited across the seam',
    starts_at: FIRST_PASS_0210,
    ends_at: FIRST_PASS_0250,
  });

  try {
    await page.goto(`/#/event/${id}/${DATE}/edit`);
    const end = page.locator('input[type="time"]').last();
    await end.fill('02:20');
    await end.blur();
    await page.getByRole('button', { name: /^Save$/ }).click();
    await expect(page).not.toHaveURL(/\/edit$/);

    // The start was never touched, so it keeps the instant it arrived with; the end was,
    // so it is read as a wall time and lands on the second pass.
    expect(await storedTimes(page, id)).toEqual({
      title: 'Edited across the seam',
      starts_at: FIRST_PASS_0210,
      ends_at: SECOND_PASS_0220,
    });
  } finally {
    await deleteEvent(page, id);
  }
});

test('a time typed inside the hour the clocks skip is normalised forward', async ({ page }) => {
  await page.goto('/');
  await assertTheHourNeverHappens(page);

  // 01:30 CET to 04:30 CEST — three hours on the clock face, two of real time, and it
  // already straddles the gap, so moving the start into the gap cannot put it after the
  // end and turn this into a test about validation.
  const id = await createEvent(page, {
    title: 'Short night',
    starts_at: SPRING_0130,
    ends_at: SPRING_0430,
  });

  try {
    await page.goto(`/#/event/${id}/${SPRING_DATE}/edit`);
    const times = page.locator('input[type="time"]');
    await expect(times.first()).toHaveValue('01:30');
    await expect(times.last()).toHaveValue('04:30');

    // 02:30 does not happen on this date: the clock goes 01:59 straight to 03:00. The
    // rule is that it moves forward by the length of the gap, so it means 03:30 — not
    // 01:30 an hour back, and not the seam at 03:00 either. That is the same answer
    // internal/events gives when it expands a 02:30 series onto this date; see
    // TestWallTimeInAnHourTheClocksBreakResolvesToOnePinnedInstant, which pins this very
    // instant. Neither test is worth much without the other.
    await times.first().fill('02:30');
    await times.first().blur();
    await page.getByRole('button', { name: /^Save$/ }).click();

    await expect(page.getByText('The end must come after the start.')).toHaveCount(0);
    await expect(page).not.toHaveURL(/\/edit$/);

    expect(await storedTimes(page, id, SPRING_DATE)).toEqual({
      title: 'Short night',
      starts_at: NORMALISED_0230,
      ends_at: SPRING_0430, // untouched, so it keeps the instant it was loaded with
    });

    // And that instant really is half past three, not half past two.
    expect(await parisWall(page, [NORMALISED_0230, SPRING_SEAM])).toEqual(['03:30', '03:00']);
  } finally {
    await deleteEvent(page, id);
  }
});
