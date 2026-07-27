// A timezone the server accepts and the browser does not must say so.
//
// The two halves of this application read different copies of the tz database. The
// server validates ALMANACK_TZ with time.LoadLocation against the operating system's
// copy; the browser resolves the same name through Intl, against whatever its engine
// shipped with. A zone young enough to be in one and not the other — America/Coyhaique
// arrived in tzdata 2025a — therefore starts the server happily and lands in the
// browser as a bare "RangeError: Invalid time zone specified". Every date operation in
// the app goes through wallParts() in js/dates.js, so the first one to run threw, and
// what a household saw was a spinner that never became a calendar.
//
// The zone is stubbed rather than configured because the server under test is the
// shared seeded one, and because no real zone name can be relied on to stay missing
// from a browser — the whole point of the bug is that browsers catch up, and a test
// pinned to Coyhaique would quietly stop testing anything the year Chromium learns it.
// So Intl.DateTimeFormat is made to refuse this server's own zone in exactly the way a
// browser without it would: same constructor, same error type, same message.
//
// This lives in e2e/ because the fault is in a browser's tz database and the repair is
// a screen. There is no JavaScript unit runner to put it in — the frontend takes no
// dependencies (CONVENTIONS §1) — and a stub under node proves the throw is named
// without proving anybody is ever shown it.

import { test, expect } from '@playwright/test';

// A service worker that has claimed the page would answer /api/v1/config and the locale
// out of its own cache, which is one more thing standing between this test and what it
// is asking about. Nothing here writes, so there is nothing worth caching either.
test.use({ serviceWorkers: 'block' });

/** The zone this server is actually configured with, read rather than written down. */
async function configuredZone(page) {
  const cfg = await (await page.request.get('/api/v1/config')).json();
  return cfg.family_tz;
}

/** Make this browser one that has never heard of `tz`, from before the app loads. */
async function forgetZone(page, tz) {
  await page.addInitScript((zone) => {
    const Real = Intl.DateTimeFormat;
    const Fake = function DateTimeFormat(locales, options) {
      if (options && options.timeZone === zone) {
        throw new RangeError(`Invalid time zone specified: ${zone}`);
      }
      return new Real(locales, options);
    };
    // Everything else about Intl has to keep working: the app formats a 12-hour clock
    // and sorts titles through it, and a stub that broke those would be testing itself.
    Fake.prototype = Real.prototype;
    Fake.supportedLocalesOf = Real.supportedLocalesOf.bind(Real);
    Intl.DateTimeFormat = Fake;
  }, tz);
}

test('a timezone this browser cannot resolve explains itself instead of rendering nothing', async ({ page }) => {
  const zone = await configuredZone(page);
  expect(zone).toBeTruthy();

  // An uncaught error is how this was reported, so it is collected rather than assumed
  // away.
  const crashes = [];
  page.on('pageerror', (err) => crashes.push(String(err)));

  await forgetZone(page, zone);
  await page.goto('/');

  // The load-bearing assertion: the name of the zone is on the screen. Everything an
  // operator has to do follows from knowing which setting is wrong.
  const message = page.getByRole('alert');
  await expect(message).toBeVisible();
  await expect(message).toContainText(zone);
  await expect(message).toContainText('ALMANACK_TZ');

  // And no calendar. A wrong hour a family believes is worse than no hour at all, so
  // the app must not have fallen back to UTC or to the device zone and carried on.
  await expect(page.getByRole('button', { name: /Today/i })).toHaveCount(0);
  await expect(page.getByRole('button', { name: /Settings/i })).toHaveCount(0);

  expect(crashes).toEqual([]);
});

test('the refusal comes before the login form, and offers the way back', async ({ page, context }) => {
  await context.clearCookies();
  const zone = await configuredZone(page);
  await forgetZone(page, zone);

  // Signed out, the old code reached the login form — which works right up until it
  // succeeds: reading /me sets the cursor to today, and there is no today in a zone
  // that does not resolve. Somebody typing the right password was handed the login
  // screen again, and would have gone on being handed it.
  await page.goto('/');
  await expect(page.getByRole('alert')).toContainText(zone);
  await expect(page.getByRole('button', { name: /Sign in/i })).toHaveCount(0);

  // Reloading is the whole recovery: /config is read again on every boot, so correcting
  // the server is all it takes. The stub is still installed, so the same screen comes
  // back either way — a mark on the document that a reload would not survive is what
  // separates "reloaded" from "did nothing at all".
  await page.evaluate(() => { window.__stillTheSameDocument = true; });
  await page.getByRole('button', { name: /Retry|Réessayer/i }).click();
  await expect(page.getByRole('alert')).toContainText(zone);
  expect(await page.evaluate(() => window.__stillTheSameDocument)).toBeUndefined();
});
