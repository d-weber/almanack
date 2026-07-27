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
// Run against a freshly seeded dev server:  make seed && make dev

import { test, expect } from '@playwright/test';

test('the seeded family calendar loads and shows its events', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByText("Leo's dentist")).toBeVisible();
  await expect(page.getByText('Swimming').first()).toBeVisible();
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
  await page.getByLabel(/Title/i).fill('Test meeting');
  await page.getByRole('button', { name: /Save/i }).click();
  await expect(page.getByText('Test meeting')).toBeVisible();
});

test('a hostile event title is rendered as text, never as markup', async ({ page }) => {
  const hostile = '<img src=x onerror="window.__pwned = true">';
  await page.goto('/');

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
