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
// The zone reaches the browser from two places, and that is what most of this file is
// about. /config carries it at boot, and /me carries it again at every sign-in and
// every refresh. Checking only the first left the worst of the bug entirely intact: one
// /config that did not answer — a server still starting, a proxy blip, a 5xx — and the
// app ran on the built-in Europe/Paris, which every browser resolves, past the check,
// and met the real zone in /me instead. The throw came out of loadSession() into a catch
// that reads a failure there as "not signed in", so the login form came back with every
// POST /auth/login answering 200 and nothing anywhere saying why.
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
import { CREDENTIALS, HEADERS, signIn } from './fixtures.js';

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

/**
 * Fail the next GET /api/v1/config, and only the next one.
 *
 * This is the whole of the setup for the login loop. It is not an exotic condition: a
 * server that has not finished starting, a reverse proxy between restarts, one 5xx.
 * Later boots are let through so the Retry button still means what it says.
 */
async function dropTheFirstConfig(page) {
  let dropped = false;
  await page.route('**/api/v1/config', (route) => {
    if (dropped) return route.continue();
    dropped = true;
    return route.abort('failed');
  });
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

  // The refusal replaces the shell rather than filling the main area, and that is
  // asserted against the shell's own elements because the two lines above cannot see
  // it: the router never starts here, so there are no tabs to find either way, and
  // mounting the message into #view instead passed every assertion above while leaving
  // the sidebar and the tab bar standing around a message saying there is no calendar.
  await expect(page.locator('#view')).toHaveCount(0);
  await expect(page.locator('.tabbar')).toHaveCount(0);
  await expect(page.locator('#app > .fatal-card')).toHaveCount(1);
  // Including the class. `.app` is what makes that element the shell — a two-column
  // grid above 900px — so leaving it on is leaving the shell on.
  await expect(page.locator('#app')).not.toHaveClass(/(^|\s)app(\s|$)/);

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

test('the right password does not hand back the login form when /config never answered', async ({ page, context }) => {
  await context.clearCookies();
  const zone = await configuredZone(page);
  await forgetZone(page, zone);
  await dropTheFirstConfig(page);

  // With no /config the app is still on its built-in Europe/Paris, which resolves, so
  // the boot check finds nothing wrong and the login form is drawn — correctly, there
  // is no session. Everything from here is real: the real form, the real password, the
  // real /auth/login, the real /me.
  await page.goto('/');
  await expect(page.getByRole('button', { name: /Sign in/i })).toBeVisible();
  await signIn(page, CREDENTIALS);

  // /me answers 200 and brings the zone with it. This is where the loop was: the throw
  // out of applyMe() was read as a failed session and the form came straight back, so
  // the same password could be typed all evening. signIn() has already asserted the
  // 200, which is what makes this a loop rather than a rejection.
  await expect(page.getByRole('alert')).toContainText(zone);
  await expect(page.getByRole('alert')).toContainText('ALMANACK_TZ');
  await expect(page.getByRole('button', { name: /Sign in/i })).toHaveCount(0);
  await expect(page.getByRole('button', { name: /Today/i })).toHaveCount(0);
});

test('a session already in the cookie jar reaches the same screen, not a calendar', async ({ page }) => {
  // The other way into /me: this project carries the shared signed-in state, so boot
  // goes straight through to a 200 without anybody touching the login form.
  const zone = await configuredZone(page);
  await forgetZone(page, zone);
  await dropTheFirstConfig(page);

  await page.goto('/');

  await expect(page.getByRole('alert')).toContainText(zone);
  await expect(page.getByRole('button', { name: /Today/i })).toHaveCount(0);
});

test('the zone is named even when the catalogue is the thing that did not load', async ({ page }) => {
  const zone = await configuredZone(page);
  await forgetZone(page, zone);
  // /config answers and the locale does not, which is one of the two ways this arrives:
  // the other is an offline start where the worker's precache of the locale had failed,
  // which sw.js tolerates on purpose. t() then falls back to the key, and a key has no
  // {tz} in it — so a zone interpolated into a catalogue sentence was silently dropped
  // from the one screen that exists to name a zone.
  await page.route('**/locales/*.json', (route) => route.abort('failed'));

  await page.goto('/');

  const message = page.getByRole('alert');
  await expect(message).toContainText(zone);
  await expect(message).toContainText('ALMANACK_TZ');
  // And the catalogue really is missing, or the two lines above would be asserting
  // nothing. Untranslated keys on screen are what that looks like.
  await expect(message).toContainText('error.timezone.title');
});

test('a zone that changes under a tab that has been open for days says so, not "server error"', async ({ page }) => {
  // The admitted gap in #58, and the only one where the app is already running: the
  // server is restarted with a different ALMANACK_TZ, app_version does not move because
  // the assets did not, so nothing prompts a reload. /me brings the new zone back with
  // the next refresh — one click on "Week starts on" is enough — and what came up was
  // the whole shell, sidebar and tab bar and "July 2026", over "Server error. Please
  // try again." and a Retry that could never work. That is the browser's own trouble
  // reported as the server's, in front of the calendar the refusal exists to withhold.
  //
  // The restart is done to the answer rather than to the server, because this suite
  // shares one. Almanack/Nowhere is not stubbed and does not need to be: no browser has
  // ever had it, which is the whole of the condition. Only GET is rewritten, so the
  // PATCH that triggers the refresh is the real one.
  const NOWHERE = 'Almanack/Nowhere';

  await page.goto('/#/settings');
  await expect(page.getByLabel(/Week starts on/i)).toBeVisible();
  await expect(page.locator('.tabbar')).toHaveCount(1);

  const before = (await (await page.request.get('/api/v1/me')).json()).user.week_start;
  await page.route('**/api/v1/me', async (route) => {
    if (route.request().method() !== 'GET') return route.continue();
    const response = await route.fetch();
    const body = await response.json();
    body.family_tz = NOWHERE;
    return route.fulfill({ response, json: body });
  });

  try {
    await page.getByLabel(/Week starts on/i).selectOption(String(before === 1 ? 0 : 1));

    // The shell first, because the settings screen has an alert of its own: matching
    // that one is how this could pass while everything it is about was still standing.
    await expect(page.locator('.tabbar')).toHaveCount(0);
    await expect(page.getByText('Server error')).toHaveCount(0);

    const message = page.getByRole('alert');
    await expect(message).toContainText(NOWHERE);
    await expect(message).toContainText('ALMANACK_TZ');
  } finally {
    // The preference is this family's, and every other spec reads the month grid it
    // lays out. Put it back through the API: the page is a refusal screen by now.
    await page.request.patch('/api/v1/me', { headers: HEADERS, data: { week_start: before } });
  }
});

test('the refusal is a screen, not a card wedged into the sidebar column', async ({ page }) => {
  const zone = await configuredZone(page);
  await forgetZone(page, zone);
  await page.setViewportSize({ width: 1280, height: 800 });

  await page.goto('/');

  const card = page.locator('.fatal-card');
  await expect(card).toBeVisible();
  const box = await card.boundingBox();
  const view = page.viewportSize();

  // Above 900px the shell is `display: grid` with a 232px first column and named areas
  // for the sidebar, the bar and the view. A card mounted into it while it still called
  // itself `.app` had no grid-area of its own, auto-placed into the sidebar column, and
  // came out 204px wide at (14, 24) with its lines 154px across — in both themes, on
  // every desktop browser, while the CSS comment claimed it carried its own centring.
  // Text and roles cannot see any of that, which is why this measures.
  expect(box.width).toBeGreaterThan(400);
  expect(Math.abs((box.x + box.width / 2) - view.width / 2)).toBeLessThanOrEqual(1);
  expect(box.y).toBeGreaterThan(100);
});
