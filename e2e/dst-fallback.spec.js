// The one hour a year that exists twice.
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

const HEADERS = { 'X-Requested-With': 'almanack' };

/**
 * The hour really is ambiguous in this browser's tzdata.
 *
 * Without this the tests below would still pass on a browser whose Paris had no
 * fall-back at all — they would simply have stopped testing anything. If this ever
 * fails, the changeover has been abolished and the constants above need a date that
 * still has one, not a weaker assertion.
 */
async function assertTheHourExistsTwice(page) {
  const wall = await page.evaluate((instants) => instants.map((i) =>
    new Intl.DateTimeFormat('en-GB', { timeZone: 'Europe/Paris', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })
      .format(new Date(i))), [FIRST_PASS_0210, SECOND_PASS_0210]);
  expect(wall, `${FIRST_PASS_0210} and ${SECOND_PASS_0210} must both be 02:10 in Paris`)
    .toEqual(['02:10', '02:10']);
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

/** What the server holds now, which is the only thing that settles either of these. */
async function storedTimes(page, id) {
  const body = await (await page.request.get(`/api/v1/events/${id}?date=${DATE}`)).json();
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
