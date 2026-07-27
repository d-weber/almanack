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

import { test as setup, expect } from '@playwright/test';

const STATE_PATH = '.auth/state.json';

const CREDENTIALS = { email: 'mum@example.org', password: 'password' };

setup('sign in through the login form', async ({ page }) => {
  await page.goto('/');

  await page.getByLabel(/Email address/i).fill(CREDENTIALS.email);
  await page.getByLabel(/Password/i).fill(CREDENTIALS.password);
  await page.getByRole('button', { name: /Sign in/i }).click();

  // The month view's Today button is the first thing that only exists once the
  // session cookie has been accepted and /api/v1/me has answered.
  await expect(page.getByRole('button', { name: /Today/i })).toBeVisible();

  await page.context().storageState({ path: STATE_PATH });
});
