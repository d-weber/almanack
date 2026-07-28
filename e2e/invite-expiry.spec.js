// An invite's expiry day is read in the family's timezone, not off the front of the text.
//
// `expires_at` is an instant: seven days after the invite was made, to the minute, so it
// carries whatever time of day that was. Taking the first ten characters of the UTC text
// is the same day as the family's for most of the day and the day before it after 22:00
// or 23:00 Paris time, depending on the season — so a link made in the evening was
// advertised as expiring a day early, and the family would have believed it.
//
// #64 fixed this in two places at once: the search results, which have a spec of their
// own, and here — the invite list on a calendar's own screen, and the dialog that comes
// up when a link is made. Reverting these two back to `String(expires_at).slice(0, 10)`
// was green across the whole project, which is what this file is for. Half a shipped fix
// with nothing holding it is the half that comes back.
//
// Both cases are set up by answering the invite endpoints rather than by asking the
// server for a real invite at a real time: the instant has to be one whose UTC day and
// Paris day differ, and waiting until half past ten at night is not a test. The dialog
// case is answered rather than passed through for a second reason — a POST that reached
// the server would mint a real invite into the seeded family, and this file has nothing
// to revoke it with that would not itself be under test.

import { test, expect } from '@playwright/test';

// Both tests answer an API request instead of the server, and a service worker that has
// claimed the page answers /api/ out of its own cache before anything here is consulted —
// sw.js calls skipWaiting() and clients.claim(), so it takes control partway through a
// visit rather than on the next one. Requests it makes are its own, not the page's, and
// page.route never sees them: whichever of the two won the race decided whether these
// tests were looking at the invite they set up or at the family's real (empty) list.
// Nothing here writes, so there is nothing worth caching either.
test.use({ serviceWorkers: 'block' });

// 00:30 on the 5th in Paris (CEST, UTC+2), which is still the 4th in UTC. The one thing
// that must not appear on screen is 4 August.
const LATE_EVENING = '2026-08-04T22:30:00Z';
const IN_PARIS = '5 Aug 2026';
const IN_UTC = '4 Aug 2026';

/** The first calendar this family has, which is the one the screens below open on. */
async function firstCalendarId(page) {
  const me = await (await page.request.get('/api/v1/me')).json();
  return me.calendars[0].id;
}

test('an invite made late in the evening lists the day it really expires', async ({ page }) => {
  const id = await firstCalendarId(page);

  await page.route('**/api/v1/calendars/*/invites', async (route) => {
    if (route.request().method() !== 'GET') return route.continue();
    return route.fulfill({ json: { invites: [{ id: 4242, expires_at: LATE_EVENING }] } });
  });

  await page.goto(`/#/calendars/${id}`);

  await expect(page.getByText(`Expires on ${IN_PARIS}`)).toBeVisible();
  await expect(page.getByText(IN_UTC)).toHaveCount(0);
});

test('and the dialog that hands over the link says the same day the list does', async ({ page }) => {
  const id = await firstCalendarId(page);

  await page.route('**/api/v1/calendars/*/invites', async (route) => {
    const method = route.request().method();
    if (method === 'POST') {
      return route.fulfill({
        status: 201,
        json: {
          id: 4243,
          token: 'a'.repeat(43),
          url: 'https://example.invalid/#/join/' + 'a'.repeat(43),
          expires_at: LATE_EVENING,
        },
      });
    }
    if (method !== 'GET') return route.continue();
    return route.fulfill({ json: { invites: [] } });
  });

  await page.goto(`/#/calendars/${id}`);
  await page.getByRole('button', { name: /^Invite$/ }).click();

  const dialog = page.locator('.dialog');
  await expect(dialog).toBeVisible();
  await expect(dialog.getByText(`Expires on ${IN_PARIS}`)).toBeVisible();
  await expect(dialog.getByText(IN_UTC)).toHaveCount(0);
});
