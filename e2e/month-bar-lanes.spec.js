// An all-day event takes space from the days it covers, and from no others.
//
// The month grid draws spanning bars for all-day and multi-day events, and timed events
// as chips underneath. Bars and chips used to be two stacked blocks, so the bar band
// reserved its full height across all seven columns: one all-day event on Thursday
// pushed Monday's, Tuesday's and Wednesday's timed events down with it, in a row whose
// height is budgeted to the pixel. Two lanes cost every day in the week 50px.
//
// Both directions are asserted here, because the obvious fix breaks the other one: a day
// the bar really does cover still has to clear it, or the chip is drawn over the bar.
//
// The fixtures are created through the API and deleted in a finally, whether the
// assertions passed or not — see the header of smoke.spec.js for what leaving them
// behind costs the specs that come after.

import { test, expect } from '@playwright/test';
import { HEADERS } from './fixtures.js';

// A month far from the seeded family's events, so nothing else lands in this row.
// 1 March 2027 is a Monday, which is where the seeded account's week starts.
const MONDAY = '2027-03-01';
const WEDNESDAY = '2027-03-03';
const ANCHOR = '2027-03-15';

const ALL_DAY = 'Lane test all-day Wed';
const ON_COVERED = 'Lane test timed Wed';
const ON_CLEAR = 'Lane test timed Mon';

/** The chip carrying this title, and the bar, measured against the week row they share. */
async function geometry(page, chipTitle) {
  return page.evaluate((title) => {
    const chip = [...document.querySelectorAll('.chip')].find((c) => c.textContent.includes(title));
    if (!chip) return null;
    const row = chip.closest('.week-row');
    const bar = row && row.querySelector('.bar');
    if (!bar) return null;
    const c = chip.getBoundingClientRect();
    const b = bar.getBoundingClientRect();
    return {
      chipTop: Math.round(c.top),
      barTop: Math.round(b.top),
      barBottom: Math.round(b.bottom),
      // Whether they share any column at all, which is what decides the rule below.
      sameColumn: c.left < b.right - 0.5 && c.right > b.left + 0.5,
    };
  }, chipTitle);
}

test('an all-day event moves down only the days it covers', async ({ page }) => {
  const created = [];
  const add = async (body) => {
    const res = await page.request.post('/api/v1/events', { headers: HEADERS, data: body });
    expect(res.status(), await res.text()).toBe(201);
    created.push((await res.json()).event.id);
  };

  try {
    await add({
      calendar_id: 1, title: ALL_DAY, all_day: true,
      start_date: WEDNESDAY, end_date: WEDNESDAY, label_id: 1,
    });
    await add({
      calendar_id: 1, title: ON_COVERED, all_day: false,
      starts_at: `${WEDNESDAY}T09:00:00Z`, ends_at: `${WEDNESDAY}T10:00:00Z`, label_id: 2,
    });
    await add({
      calendar_id: 1, title: ON_CLEAR, all_day: false,
      starts_at: `${MONDAY}T09:00:00Z`, ends_at: `${MONDAY}T10:00:00Z`, label_id: 2,
    });

    await page.goto(`/#/month?d=${ANCHOR}`);
    await expect(page.getByText(ALL_DAY).first()).toBeVisible({ timeout: 15_000 });

    const covered = await geometry(page, ON_COVERED);
    const clear = await geometry(page, ON_CLEAR);
    expect(covered, 'the Wednesday chip and the bar were not found in one week row').not.toBeNull();
    expect(clear, 'the Monday chip and the bar were not found in one week row').not.toBeNull();

    // Wednesday is under the bar, so its chip is drawn below it. Without this the fix
    // for the other half would put the chip on top of the bar.
    expect(covered.sameColumn, 'the all-day bar is not above the Wednesday chip').toBe(true);
    expect(
      covered.chipTop,
      "Wednesday's timed event is drawn over the all-day bar covering that day",
    ).toBeGreaterThanOrEqual(covered.barBottom);

    // Monday has nothing all-day on it, so it loses nothing to Wednesday's bar: its chip
    // starts level with the bar rather than below the band.
    expect(clear.sameColumn, 'the bar unexpectedly spans Monday too').toBe(false);
    expect(
      clear.chipTop,
      "Monday's timed event was pushed down by an all-day event on another day",
    ).toBe(clear.barTop);
  } finally {
    for (const id of created) {
      await page.request.delete(`/api/v1/events/${id}?scope=all`, { headers: HEADERS });
    }
  }
});

// The longest bar takes the top lane, whatever kind it is.
//
// A week-long trip drawn under a one-day holiday reads as though the holiday were the
// longer of the two, and the trip's own row then changes height from day to day as
// shorter bars come and go beneath it. On the demo calendar this showed exactly as
// reported: the Seaside holiday sitting under Bastille Day.
//
// A public holiday is the case worth testing rather than two ordinary events, because
// holidays used to be laid out first *unconditionally* — that was the rule this changes.
// Two ordinary events would not exercise it at all: they were already ordered by where
// they start, so a trip beginning on the Monday took the top lane either way.
//
// Holidays now win a tie rather than the top outright: above an ordinary event of the
// same length, below one that spans more of the week.
//
// July 2027 is used because the demo family's events sit near today and Bastille Day
// falls on Wednesday the 14th there, well clear of them.
test('a trip spanning a public holiday is drawn above it', async ({ page }) => {
  const created = [];
  const TRIP = 'Lane order long trip';
  const BASTILLE = 'Bastille Day';

  try {
    const res = await page.request.post('/api/v1/events', {
      headers: HEADERS,
      data: {
        calendar_id: 1, title: TRIP, all_day: true,
        start_date: '2027-07-12', end_date: '2027-07-16', label_id: 1,
      },
    });
    expect(res.status(), await res.text()).toBe(201);
    created.push((await res.json()).event.id);

    await page.goto('/#/month?d=2027-07-15');
    await expect(page.getByTitle(TRIP).first()).toBeVisible({ timeout: 15_000 });
    await expect(page.getByTitle(BASTILLE).first()).toBeVisible();

    const tops = await page.evaluate(({ trip, holiday }) => {
      const top = (title) => {
        const bar = document.querySelector(`.bar[title="${title}"]`);
        return bar ? Math.round(bar.getBoundingClientRect().top) : null;
      };
      return { trip: top(trip), holiday: top(holiday) };
    }, { trip: TRIP, holiday: BASTILLE });

    expect(tops.trip, `no bar for ${TRIP}`).not.toBeNull();
    expect(tops.holiday, `no bar for ${BASTILLE}`).not.toBeNull();
    expect(
      tops.trip,
      'the five-day trip is drawn below the one-day public holiday it spans',
    ).toBeLessThan(tops.holiday);
  } finally {
    for (const id of created) {
      await page.request.delete(`/api/v1/events/${id}?scope=all`, { headers: HEADERS });
    }
  }
});
