// Sign in once, and let every other spec reuse the session.
//
// This exists because signing in per test does not scale past eight of them: the login
// endpoint's token bucket has a burst of 8 (internal/httpapi/ratelimit.go), and the
// whole suite arrives from one address, so the ninth sign-in gets a 429 and every test
// after it fails at the password box for a reason that has nothing to do with what it
// was testing. Rate limiting a family calendar's login endpoint is correct; hammering
// it ten times to assert something about the month grid is not.
//
// So almost everything starts already authenticated, and only three files call signIn():
// this one, logout.spec.js — which needs a session it is allowed to destroy — and
// unknown-tz.spec.js, whose whole subject is what a correct password does next. Three of
// a burst of eight, emptied below at the start of every run.
//
// Two sign-ins a run was still too many, though, because the bucket does not empty
// between runs and refills at one token per twenty seconds: five `make e2e` runs inside
// a minute spent the burst of eight, and the sixth failed here and in logout.spec.js at
// the password box, with `toBeVisible() failed` and nothing anywhere saying 429 (#66).
// Restarting the server cleared it, since the buckets are in memory. So this step now
// asks the server for a restart's effect on the buckets without the restart, before
// spending anything: `make seed` gives the run a clean database, and this gives it a
// clean bucket. The limits themselves are untouched — the endpoint is registered only in
// dev mode, which is what the suite runs against, and emptying a bucket on request is a
// developer's affordance, not a weaker limiter.
//
// It is also where the run says, once, whether the database it has been pointed at is
// the one `make seed` makes. See below for why that is worth a step of its own.

import { test as setup, expect } from '@playwright/test';
import { CREDENTIALS, FIXTURE_TITLES, HEADERS, RATE_LIMIT_RESET_PATH, signIn } from './fixtures.js';

const STATE_PATH = '.auth/state.json';

setup('sign in through the login form, on a database nobody has run this on before', async ({ page }) => {
  const reset = await page.request.post(RATE_LIMIT_RESET_PATH, { headers: HEADERS });
  expect(
    reset.status(),
    `${RATE_LIMIT_RESET_PATH} answered ${reset.status()}.\n` +
      'This suite runs against `make seed && make dev`, which sets ALMANACK_DEV=1; a server ' +
      'started without it registers no /dev routes at all, and the login bucket this run is ' +
      'about to spend two tokens of can then only be emptied by restarting the server.',
  ).toBe(200);

  await page.goto('/');
  await signIn(page, CREDENTIALS);

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
