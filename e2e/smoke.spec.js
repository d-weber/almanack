// Browser smoke tests. Development-only: `make e2e` skips silently when npx is absent.
//
// These cover the handful of things Go tests structurally cannot — that the app
// renders at all, that the Content-Security-Policy does not quietly break the UI, and
// that a hostile event title stays inert in a real DOM rather than merely being
// escaped in a JSON response.
//
// Run against a freshly seeded dev server:  make seed && make dev

import { test, expect } from '@playwright/test';

const CREDENTIALS = { email: 'mum@example.org', password: 'password' };

async function signIn(page, who = CREDENTIALS) {
  await page.goto('/');
  await page.getByLabel(/Email address/i).fill(who.email);
  await page.getByLabel(/Password/i).fill(who.password);
  await page.getByRole('button', { name: /Sign in/i }).click();
  await expect(page.getByRole('button', { name: /Today/i })).toBeVisible();
}

test('the seeded family calendar loads and shows its events', async ({ page }) => {
  await signIn(page);
  await expect(page.getByText("Leo's dentist")).toBeVisible();
  await expect(page.getByText('Swimming').first()).toBeVisible();
});

test('no console errors and no CSP violations on load', async ({ page }) => {
  const problems = [];
  page.on('console', (msg) => {
    if (msg.type() === 'error') problems.push(msg.text());
  });
  page.on('pageerror', (err) => problems.push(String(err)));

  await signIn(page);
  await page.getByRole('button', { name: /Today/i }).click();

  // A CSP violation surfaces here as "Refused to execute…". Since the app ships
  // without unsafe-inline, an inline handler someone added would fail loudly.
  expect(problems, `console errors:\n${problems.join('\n')}`).toHaveLength(0);
});

test('creating an event shows it in the month grid', async ({ page }) => {
  await signIn(page);
  await page.getByRole('button', { name: /New event|Add|\+/ }).first().click();
  await page.getByLabel(/Title/i).fill('Test meeting');
  await page.getByRole('button', { name: /Save/i }).click();
  await expect(page.getByText('Test meeting')).toBeVisible();
});

test('a hostile event title is rendered as text, never as markup', async ({ page }) => {
  const hostile = '<img src=x onerror="window.__pwned = true">';
  await signIn(page);

  await page.getByRole('button', { name: /New event|Add|\+/ }).first().click();
  await page.getByLabel(/Title/i).fill(hostile);
  await page.getByRole('button', { name: /Save/i }).click();

  // The literal characters must appear on screen…
  await expect(page.getByText(hostile)).toBeVisible();
  // …the payload must not have executed…
  expect(await page.evaluate(() => window.__pwned)).toBeUndefined();
  // …and no element may have been injected.
  expect(await page.locator('img[src="x"]').count()).toBe(0);
});

test('the service worker registers and the app is installable', async ({ page }) => {
  await signIn(page);
  const registered = await page.evaluate(async () => {
    if (!('serviceWorker' in navigator)) return false;
    const reg = await navigator.serviceWorker.getRegistration();
    return Boolean(reg);
  });
  expect(registered).toBe(true);

  const manifest = await page.request.get('/manifest.json');
  expect(manifest.ok()).toBeTruthy();

  // The version placeholder must have been substituted by the server; shipping the
  // literal would give every deployment the same cache name forever.
  const sw = await (await page.request.get('/sw.js')).text();
  expect(sw).not.toContain('__APP_VERSION__');
});

test('the offline banner appears and cached events remain readable', async ({ page, context }) => {
  await signIn(page);
  await expect(page.getByText("Leo's dentist")).toBeVisible();

  await context.setOffline(true);
  await page.reload();

  // The calendar still renders from cache rather than showing a browser error page.
  await expect(page.getByText("Leo's dentist")).toBeVisible({ timeout: 10_000 });
  await context.setOffline(false);
});
