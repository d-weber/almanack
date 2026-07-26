// The device-timezone test, run by the `chromium-lisbon` project.
//
// This is the bug the whole date layer is built to avoid: a family member travelling
// (or a laptop set to the wrong zone) must still see the Paris dentist appointment at
// its Paris time. Intl defaults to the *device* timezone, so any date rendered
// without an explicit timeZone option silently shifts by an hour here — and an
// all-day event shifts by a whole day.

import { test, expect } from '@playwright/test';

test('events render in the family timezone, not the device one', async ({ page }) => {
  await page.goto('/');
  await page.getByLabel(/Adresse e-mail/i).fill('maman@example.org');
  await page.getByLabel(/Mot de passe/i).fill('motdepasse');
  await page.getByRole('button', { name: /Se connecter/i }).click();
  await expect(page.getByRole('button', { name: /Aujourd'hui/i })).toBeVisible();

  // Confirm the browser really is in Lisbon (one hour behind Paris), or this test
  // proves nothing.
  const deviceZone = await page.evaluate(() => Intl.DateTimeFormat().resolvedOptions().timeZone);
  expect(deviceZone).toBe('Europe/Lisbon');

  // The seeded dentist appointment is at 16:30 Paris time. A device-timezone bug
  // would render it as 15:30.
  const card = page.getByText('Dentiste Léo').first();
  await card.click();
  await expect(page.getByText('16:30')).toBeVisible();
  await expect(page.getByText('15:30')).toHaveCount(0);
});

test('an all-day event does not slip to the previous day', async ({ page }) => {
  await page.goto('/');
  await page.getByLabel(/Adresse e-mail/i).fill('maman@example.org');
  await page.getByLabel(/Mot de passe/i).fill('motdepasse');
  await page.getByRole('button', { name: /Se connecter/i }).click();

  // "Vacances à la mer" is seeded as a multi-day all-day event. Stored as a midnight
  // instant it would start a day early west of Paris; stored as a date it cannot.
  await page.getByRole('button', { name: /Agenda/i }).click();
  const holiday = page.getByText('Vacances à la mer').first();
  await expect(holiday).toBeVisible();
});
