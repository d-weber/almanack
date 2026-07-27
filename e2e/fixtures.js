// What more than one file in this suite needs: the titles the specs create, the CSRF
// header, and the sign-in the two files that sign in both go through.

import { expect } from '@playwright/test';

// Event titles the smoke tests create through the editor.
//
// They live here rather than inline because two files need them: the specs that create
// them, and the clean-start check in auth.setup.js that looks for them left over from an
// earlier run. Put a title in one place and the check goes on passing over a database
// that is not clean, which is the failure this whole arrangement exists to prevent.
//
// Only the ones created through the UI are listed. A fixture created through the API is
// deleted by id in a finally, and has been since it was written; these two had nothing to
// delete by until they existed, which is how they came to be left behind.

export const MEETING = 'Test meeting';

export const HOSTILE_TITLE = '<img src=x onerror="window.__pwned = true">';

export const FIXTURE_TITLES = [MEETING, HOSTILE_TITLE];

/** Mutations carry the CSRF header or the middleware refuses them. */
export const HEADERS = { 'X-Requested-With': 'almanack' };

/** Where the login form posts, and the one bucket this suite spends. */
export const LOGIN_PATH = '/api/v1/auth/login';

/** Emptying the buckets, which dev mode allows and nothing else does. */
export const RATE_LIMIT_RESET_PATH = '/dev/ratelimits/reset';

const RATE_LIMITED =
  `${LOGIN_PATH} answered 429: this address has spent the login rate limiter's burst of 8 ` +
  '(internal/httpapi/ratelimit.go), which refills at one token per 20 seconds.\n' +
  `The suite spends two per run and empties the bucket at the start of each one, through ` +
  `${RATE_LIMIT_RESET_PATH} in auth.setup.js — so this means either that step did not run ` +
  'against this server, or the suite now signs in more times in one run than the burst allows.\n' +
  'Restarting the server also clears it: the buckets are in memory.';

/**
 * Fill in the login form and submit it, saying what the server answered when it refuses.
 *
 * The status of the login request is asserted here rather than left to whatever the
 * caller expects to see afterwards, because a refused sign-in and a broken application
 * looked identical from the outside: the form stays on screen either way, so the next
 * expectation — the Today button, the dentist — times out with `toBeVisible() failed`
 * against an element that was never going to appear. That is how #66 presented, in two
 * specs at once, with the actual reason visible only in the server's log. A 429 now says
 * it is a 429, and says what to do about it.
 */
export async function signIn(page, { email, password }) {
  await page.getByLabel(/Email address/i).fill(email);
  await page.getByLabel(/Password/i).fill(password);

  // Subscribed before the click, or the answer can arrive before anything is listening.
  const answer = page.waitForResponse((res) => new URL(res.url()).pathname === LOGIN_PATH);
  await page.getByRole('button', { name: /Sign in/i }).click();

  const status = (await answer).status();
  expect(status, status === 429 ? RATE_LIMITED : `${LOGIN_PATH} answered ${status}`).toBe(200);
}
