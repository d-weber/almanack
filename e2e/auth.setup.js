// Sign in once, and let every other spec reuse the session.
//
// This exists because signing in per test does not scale past eight of them: the login
// endpoint's token bucket has a burst of 8 (internal/httpapi/ratelimit.go), and the
// whole suite arrives from one address, so the ninth sign-in gets a 429 and every test
// after it fails at the password box for a reason that has nothing to do with what it
// was testing. Rate limiting a family calendar's login endpoint is correct; hammering
// it ten times to assert something about the month grid is not.
//
// So this file is also the one place the real login form is exercised. Everything else
// starts already authenticated, which is why the specs no longer call signIn().
//
// It is also where the run says, once, whether the database it has been pointed at is
// the one `make seed` makes. See below for why that is worth a step of its own.

import { test as setup, expect } from '@playwright/test';
import { FIXTURE_TITLES } from './fixtures.js';

const STATE_PATH = '.auth/state.json';

const CREDENTIALS = { email: 'mum@example.org', password: 'password' };

setup('sign in through the login form, on a database nobody has run this on before', async ({ page }) => {
  await page.goto('/');

  await page.getByLabel(/Email address/i).fill(CREDENTIALS.email);
  await page.getByLabel(/Password/i).fill(CREDENTIALS.password);
  await page.getByRole('button', { name: /Sign in/i }).click();

  // The month view's Today button is the first thing that only exists once the
  // session cookie has been accepted and /api/v1/me has answered.
  await expect(page.getByRole('button', { name: /Today/i })).toBeVisible();

  await page.context().storageState({ path: STATE_PATH });

  // And, in the one place that runs once per run, ask whether this database has been used
  // before.
  //
  // Every spec that creates an event deletes it again in a finally, so in the ordinary
  // course nothing survives a run. A run that is interrupted, though — Ctrl-C, a crash, a
  // laptop that closes — leaves one behind, and the tests that fail next time are not the
  // ones that created it: the offline test, the cache-cap test and the timezone test go red
  // over what is on screen and how many API ranges were cached, none of them says anything
  // about an extra event, and the app looks broken when the database is merely dirty. One
  // line naming the cause is worth more than three failures that point away from it.
  for (const title of FIXTURE_TITLES) {
    const found = await (await page.request.get(`/api/v1/search?q=${encodeURIComponent(title)}`)).json();
    expect(
      found.results,
      `the event "${title}" is left over from an earlier run of this suite.\n` +
        'These tests need a freshly seeded database: run `make seed`, restart the server, and try again.',
    ).toHaveLength(0);
  }
});
