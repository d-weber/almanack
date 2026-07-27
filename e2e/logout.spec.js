// Signing out has to take the cached calendar off the device with it.
//
// The service worker keeps the last-seen `/api/` responses so the app reads offline. Left
// behind after a sign-out they are the family's calendar, still on the phone, readable by
// whoever holds it next — and on an offline boot the cached `/api/v1/me` answers 200, so
// the app signs itself back in and renders the lot.
//
// This is the one spec that deliberately destroys a session, so it runs in a project of
// its own (`chromium-logout` in playwright.config.js): no `storageState`, and it signs in
// as Dad. It must not touch the session auth.setup.js saved — every other spec shares that
// cookie, and ending it server-side would fail all of them at their first request for a
// reason that has nothing to do with what they test. A different account in a context of
// its own means the sign-out below can only reach the session this test created, whatever
// the server does with it. That costs one extra sign-in: the login bucket allows a burst
// of eight and the suite spends two of them. It used not to give them back, which is how
// a fifth run inside a minute came to fail here at the password box (#66); auth.setup.js
// now empties the bucket at the start of every run, and signIn() below says plainly when
// it is a 429 that has stopped this test rather than a sign-out that has stopped working.
//
// CacheStorage is per-context too, so the purge asserted here cannot empty anything the
// other projects are relying on either.

import { test, expect } from '@playwright/test';
import { signIn } from './fixtures.js';

const CREDENTIALS = { email: 'dad@example.org', password: 'password' };

/** How many `/api/` responses this context is holding, across every cache it has. */
function cachedApiResponses(page) {
  return page.evaluate(async () => {
    let n = 0;
    for (const name of await caches.keys()) {
      const cache = await caches.open(name);
      const keys = await cache.keys();
      n += keys.filter((request) => new URL(request.url).pathname.startsWith('/api/')).length;
    }
    return n;
  });
}

test('signing out takes the cached calendar off the device', async ({ page, context }) => {
  await page.goto('/');
  await signIn(page, CREDENTIALS);
  await expect(page.getByText("Leo's dentist")).toBeVisible();

  // The worker has to be in control, or nothing below passes through it and this would be
  // asserting something about Chromium's HTTP cache instead. Registration is asynchronous,
  // so poll for it rather than reading it once on arrival.
  await expect
    .poll(() => page.evaluate(() => Boolean(navigator.serviceWorker.controller)), {
      timeout: 10_000,
    })
    .toBe(true);

  // On the first visit the worker claims the page after the calendar has already fetched
  // its range, so those responses never went through it. Reload once, still online, to put
  // a calendar in the cache at all.
  await page.reload();
  await expect(page.getByText("Leo's dentist")).toBeVisible();
  await expect.poll(() => cachedApiResponses(page), { timeout: 10_000 }).toBeGreaterThan(0);

  // Offline, that cache is what the calendar renders from. This is the behaviour the
  // sign-out has to take away, so prove it is there before taking it away.
  await context.setOffline(true);
  await page.reload();
  await expect(page.getByText("Leo's dentist")).toBeVisible({ timeout: 10_000 });

  // Now sign out for real, through the button, back on the network.
  await context.setOffline(false);
  await page.goto('/#/settings');
  await page.getByRole('button', { name: /Sign out/i }).click();
  await page.getByRole('button', { name: /^Confirm$/ }).click();
  await expect(page.getByLabel(/Email address/i)).toBeVisible();

  // The purge happens in the worker, on a message, so poll for it.
  await expect.poll(() => cachedApiResponses(page), { timeout: 10_000 }).toBe(0);

  // And the point of all of it: an offline boot now has nothing to render a calendar
  // from, and nothing to sign itself back in with.
  await context.setOffline(true);
  await page.reload();
  await expect(page.getByLabel(/Email address/i)).toBeVisible({ timeout: 10_000 });
  await expect(page.getByText("Leo's dentist")).toHaveCount(0);
  await context.setOffline(false);
});
