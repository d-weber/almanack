// The device-timezone tests, run by the `chromium-lisbon` project only — the config
// keeps them out of the Paris project, where the Lisbon assertion below could never
// hold and was failing for a reason that had nothing to do with the application.
//
// This is the bug the whole date layer is built to avoid: a family member travelling
// (or a laptop set to the wrong zone) must still see the Paris dentist appointment at
// its Paris time. Intl defaults to the *device* timezone, so any date rendered
// without an explicit timeZone option silently shifts by an hour here — and an
// all-day event shifts by a whole day.
//
// These start already signed in, from the session auth.setup.js saved.

import { test, expect } from '@playwright/test';

test('events render in the family timezone, not the device one', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByRole('button', { name: /Today/i })).toBeVisible();

  // Confirm the browser really is in Lisbon (one hour behind Paris), or this test
  // proves nothing.
  const deviceZone = await page.evaluate(() => Intl.DateTimeFormat().resolvedOptions().timeZone);
  expect(deviceZone).toBe('Europe/Lisbon');

  // The seeded dentist appointment is at 16:30 Paris time. A device-timezone bug
  // would render it as 15:30.
  const card = page.getByText("Leo's dentist").first();
  await card.click();
  await expect(page.getByText('16:30')).toBeVisible();
  await expect(page.getByText('15:30')).toHaveCount(0);
});

test('an all-day event does not slip to the previous day', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByRole('button', { name: /Today/i })).toBeVisible();

  // "Seaside holiday" is seeded as a multi-day all-day event. Stored as a midnight
  // instant it would start a day early west of Paris; stored as a date it cannot.
  await page.getByRole('button', { name: /Agenda/i }).click();
  const holiday = page.getByText('Seaside holiday').first();
  await expect(holiday).toBeVisible();
});
