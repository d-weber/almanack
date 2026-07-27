// Browser smoke tests. Development-only locally: `make e2e` skips silently when npx is
// absent. They do run in CI, which seeds a family and starts a server first.
//
// These cover the handful of things Go tests structurally cannot — that the app
// renders at all, that the Content-Security-Policy does not quietly break the UI, and
// that a hostile event title stays inert in a real DOM rather than merely being
// escaped in a JSON response.
//
// Every test here starts already signed in, from the session auth.setup.js saved. The
// login form itself is exercised there, once.
//
// The two tests below create an event, and both delete it again in a finally — through the
// API, and whether the assertions passed or not. They used to leave them, which cost the
// next run three failures somewhere else entirely: the offline test, the cache-cap test and
// the timezone test all read what is on the screen or count what has been cached, and an
// extra event changes both. CI never saw it, because CI seeds a database per run; it landed
// entirely on whoever ran the suite twice, which is the case `make e2e` exists for.
//
// Run against a freshly seeded dev server:  make seed && make dev

import { test, expect } from '@playwright/test';
import { MEETING, HOSTILE_TITLE, HEADERS } from './fixtures.js';

/**
 * Press Save and hand back the id of the event that was created.
 *
 * The id is taken off the response rather than out of the page: the editor goes back to
 * the month view when it saves, so there is nothing on screen that names the row, and
 * the only reason to want it is to be able to delete it again afterwards.
 */
async function saveNewEvent(page) {
  const created = page.waitForResponse(
    (r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/api/v1/events',
  );
  await page.getByRole('button', { name: /Save/i }).click();
  const response = await created;
  expect(response.status(), await response.text()).toBe(201);
  return (await response.json()).event.id;
}

/**
 * Remove it again, through the API rather than through the UI.
 *
 * Deleting an event in the browser is a flow of its own, with a confirmation and a
 * this / this and following / the whole series question behind it. Driving that here
 * would put a test of deletion inside a test about something else — where nobody would
 * look for it, and where it would fail these two the day it broke.
 */
async function deleteEvent(page, id) {
  await page.request.delete(`/api/v1/events/${id}`, { headers: HEADERS });
}

test('the seeded family calendar loads and shows its events', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByText("Leo's dentist")).toBeVisible();

  // The swimming series, which is what makes this more than a test that one row draws:
  // expanding a recurrence and drawing an occurrence that was edited away from its
  // series are things only the browser does, and the month is where they are done.
  //
  // Counted rather than found once. A single chip is also what a series that had
  // stopped repeating after its first occurrence would leave, so the assertion that
  // has to hold is that it repeats: the seed anchors the series to the first Tuesday
  // of the month and the grid holds every Tuesday of the month, so the first and the
  // third are always on it — the 21st at the very latest — with the second moved to
  // the evening under its own title and the fourth cancelled. That invariant is the
  // seed's, and it is pinned there too, on every date in a decade:
  // TestTheDemoSeriesLandsOnTheMonthTheAppOpensOn in cmd/almanack/seed_test.go. It
  // used to anchor the series to the next Tuesday after today instead, which on a
  // Tuesday is seven days out and can be past the end of a five-row grid, so this test
  // went red on whichever days the calendar fell badly and said nothing about why.
  const swimming = page.getByText('Swimming', { exact: true });
  await expect(swimming.first()).toBeVisible();
  expect(await swimming.count(), 'a weekly series draws more than one occurrence in a month').toBeGreaterThan(1);
  await expect(page.getByText('Swimming (later than usual)', { exact: true })).toBeVisible();
});

test('no console errors and no CSP violations on load', async ({ page }) => {
  const problems = [];
  page.on('console', (msg) => {
    if (msg.type() === 'error') problems.push(msg.text());
  });
  page.on('pageerror', (err) => problems.push(String(err)));

  await page.goto('/');
  await page.getByRole('button', { name: /Today/i }).click();

  // A CSP violation surfaces here as "Refused to execute…". Since the app ships
  // without unsafe-inline, an inline handler someone added would fail loudly.
  expect(problems, `console errors:\n${problems.join('\n')}`).toHaveLength(0);
});

test('creating an event shows it in the month grid', async ({ page }) => {
  await page.goto('/');
  await page.getByRole('button', { name: /New event|Add|\+/ }).first().click();
  await page.getByLabel(/Title/i).fill(MEETING);
  const id = await saveNewEvent(page);

  // In a finally, so that an assertion which fails still leaves the calendar as it found
  // it. A leftover event is not a leftover event's problem: it is three other tests going
  // red on the next run, over what is on screen and how many API ranges got cached.
  try {
    await expect(page.getByText(MEETING)).toBeVisible();
  } finally {
    await deleteEvent(page, id);
  }
});

test('a hostile event title is rendered as text, never as markup', async ({ page }) => {
  await page.goto('/');

  await page.getByRole('button', { name: /New event|Add|\+/ }).first().click();
  await page.getByLabel(/Title/i).fill(HOSTILE_TITLE);
  const id = await saveNewEvent(page);

  try {
    // The literal characters must appear on screen…
    await expect(page.getByText(HOSTILE_TITLE)).toBeVisible();
    // …the payload must not have executed…
    expect(await page.evaluate(() => window.__pwned)).toBeUndefined();
    // …and no element may have been injected.
    expect(await page.locator('img[src="x"]').count()).toBe(0);
  } finally {
    await deleteEvent(page, id);
  }
});

test('the service worker registers and the app is installable', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByRole('button', { name: /Today/i })).toBeVisible();

  // Registration is asynchronous, so poll rather than reading it once on arrival.
  await expect
    .poll(
      () =>
        page.evaluate(async () => {
          if (!('serviceWorker' in navigator)) return false;
          return Boolean(await navigator.serviceWorker.getRegistration());
        }),
      { timeout: 10_000 },
    )
    .toBe(true);

  const manifest = await page.request.get('/manifest.json');
  expect(manifest.ok()).toBeTruthy();

  // The version placeholder must have been substituted by the server; shipping the
  // literal would give every deployment the same cache name forever.
  const sw = await (await page.request.get('/sw.js')).text();
  expect(sw).not.toContain('__APP_VERSION__');
});

test('the offline banner appears and cached events remain readable', async ({ page, context }) => {
  await page.goto('/');
  await expect(page.getByText("Leo's dentist")).toBeVisible();

  // The service worker must have taken control, or there is nothing to serve the
  // reload from and this asserts only that Chromium has an HTTP cache.
  await expect
    .poll(() => page.evaluate(() => Boolean(navigator.serviceWorker.controller)), {
      timeout: 10_000,
    })
    .toBe(true);

  // Then reload once while still online. On the very first visit the worker installs
  // and claims the page *after* the calendar has already fetched its range, so those
  // responses never passed through it and nothing cached them. What the app promises
  // is that the last-seen calendar stays readable — which is about a visit whose data
  // the worker actually saw, i.e. the second one onwards.
  await page.reload();
  await expect(page.getByText("Leo's dentist")).toBeVisible();

  await context.setOffline(true);
  await page.reload();

  // The calendar still renders from cache rather than showing a browser error page.
  await expect(page.getByText("Leo's dentist")).toBeVisible({ timeout: 10_000 });
  await context.setOffline(false);
});

test('the cached API responses are capped, oldest first', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByText("Leo's dentist")).toBeVisible();
  await expect
    .poll(() => page.evaluate(() => Boolean(navigator.serviceWorker.controller)), {
      timeout: 10_000,
    })
    .toBe(true);

  // Seventy distinct ranges, one at a time so each is cached before the next is asked
  // for — this is a year of somebody scrolling back through the calendar, compressed.
  const ranges = await page.evaluate(async () => {
    const asked = [];
    for (let i = 0; i < 70; i++) {
      const from = `20${30 + Math.floor(i / 12)}-${String((i % 12) + 1).padStart(2, '0')}-01`;
      const url = `/api/v1/events?from=${from}&to=${from}`;
      const response = await fetch(url);
      if (response.ok) asked.push(url);
    }
    return asked;
  });
  expect(ranges).toHaveLength(70);

  const cached = (url) => page.evaluate(async (target) => {
    for (const name of await caches.keys()) {
      if (await (await caches.open(name)).match(target)) return true;
    }
    return false;
  }, url);

  // Exactly the cap, and not a smaller number: an eviction that takes more than the
  // excess is the failure mode this is really watching for, and it looks identical to a
  // working cap unless the count is pinned. Seventy asks plus the app's own handful,
  // trimmed to the sixty most recent, is sixty.
  await expect.poll(() => page.evaluate(async () => {
    let n = 0;
    for (const name of await caches.keys()) {
      const keys = await (await caches.open(name)).keys();
      n += keys.filter((request) => new URL(request.url).pathname.startsWith('/api/')).length;
    }
    return n;
  }), { timeout: 10_000 }).toBe(60);

  await expect.poll(() => cached(ranges[0]), { timeout: 10_000 }).toBe(false);
  expect(await cached(ranges[ranges.length - 1])).toBe(true);
});
