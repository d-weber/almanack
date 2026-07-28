// Browser regressions from reviewing and from using the built application.
//
// Each is a thing the Go tests structurally cannot reach: a link built in the browser
// from data the browser cannot correctly guess, a paging observer that survives being
// disconnected, a URL that stops the app booting before any of it runs, a URL for a view
// that no longer exists, and a control that changed size when a panel opened beside it.
//
// Every test starts already signed in, from the session auth.setup.js saved, and the
// one that creates an event deletes it again in a finally — through the API, whether
// its assertions passed or not. See the header of smoke.spec.js for why that matters
// to the specs that come after it.
//
// Run against a freshly seeded dev server:  make seed && make dev

import { test, expect } from '@playwright/test';
import { HEADERS } from './fixtures.js';

const SERIES_TITLE = 'Judo (review regression)';

/** The day of the week nothing seeded happens on, so the series below is ours alone. */
function aWeekdayOtherThanToday() {
  const today = new Date().getDay();
  return (today + 3) % 7;
}

test('an activity row for a series opens the event rather than an error', async ({ page }) => {
  // The row's link used to be built from the instant the change was made. A series is
  // only reachable on a date its rule produces, and the day somebody edits a lesson is
  // almost never the weekday that lesson falls on — so the link went to a date the
  // series does not land on, GET /events/{id} answered 404, and the feed reported the
  // event as deleted. The date now comes from the server, which is the only side that
  // can work it out: a series' anchor need not be an occurrence of its own rule.
  const weekday = aWeekdayOtherThanToday();
  const created = await page.request.post('/api/v1/events', {
    headers: HEADERS,
    data: {
      calendar_id: 1,
      title: SERIES_TITLE,
      all_day: false,
      starts_at: '2026-08-04T14:30:00Z',
      ends_at: '2026-08-04T15:30:00Z',
      label_id: 1,
      recurrence: { freq: 'weekly', interval: 1, by_weekday: [weekday] },
    },
  });
  expect(created.status(), await created.text()).toBe(201);
  const id = (await created.json()).event.id;

  try {
    await page.goto('/#/activity');

    const row = page.getByRole('button').filter({ hasText: SERIES_TITLE }).first();
    await expect(row).toBeVisible({ timeout: 15_000 });

    // The date it opens must be one the series really lands on, which is what the
    // server answering 200 rather than 404 says.
    const opened = page.waitForResponse(
      (r) => r.request().method() === 'GET' && new URL(r.url()).pathname === `/api/v1/events/${id}`,
    );
    await row.click();
    const response = await opened;
    expect(
      response.status(),
      `the feed linked to a date the series does not land on: ${response.url()}`,
    ).toBe(200);

    // And the reader is looking at the event, not at an error where it should be.
    await expect(page.getByText(SERIES_TITLE).first()).toBeVisible();
  } finally {
    await page.request.delete(`/api/v1/events/${id}?scope=all`, { headers: HEADERS });
  }
});

test('the agenda keeps paging after a failed page is retried', async ({ page }) => {
  // Both infinite-scroll screens disconnect their IntersectionObserver when a page
  // fails, so that a broken list stops asking. Retry put the sentinel back in the page
  // and loaded one more chunk — but nothing was watching the sentinel any more, so that
  // was the last chunk the visit ever loaded. Scrolling did nothing for the rest of the
  // session, with no error on screen to explain it.
  let pages = 0;
  let failNext = true;
  await page.route('**/api/v1/events?*', async (route) => {
    pages += 1;
    if (failNext) {
      failNext = false;
      await route.abort('failed');
      return;
    }
    await route.continue();
  });

  await page.goto('/#/agenda');

  // The first chunk failed, so the screen offers Retry.
  const retry = page.getByRole('button', { name: /Retry|Try again/i });
  await expect(retry).toBeVisible({ timeout: 15_000 });
  const afterFailure = pages;

  await retry.click();

  // The retry itself, which works either way: it is a direct call, not the observer.
  await expect.poll(() => pages, { timeout: 15_000 }).toBe(afterFailure + 1);
  await expect(page.locator('.agenda-day').first()).toBeVisible();

  // Scrolling to the foot of what it just loaded is what asks for the next chunk, and
  // it is the thing that stopped happening: the sentinel was back in the page with
  // nothing watching it. There is no error on screen at this point — the list simply
  // never grows again, however far the reader scrolls.
  await page.evaluate(() => {
    const foot = document.querySelector('.agenda-footer');
    if (foot) foot.scrollIntoView({ block: 'end' });
  });

  await expect
    .poll(() => pages, {
      timeout: 15_000,
      message: 'the agenda stopped paging after Retry: its observer was never re-observed',
    })
    .toBeGreaterThan(afterFailure + 1);
});

test('a malformed escape in the hash does not stop the app booting', async ({ page }) => {
  // decodeURIComponent throws a URIError on '%' alone, and it was called on every path
  // segment while matching a route. Uncaught, it came out of dispatch() — which boot
  // calls — so one bad character in the address bar rendered nothing at all: a blank
  // page, on that URL and on every reload of it.
  const errors = [];
  page.on('pageerror', (err) => errors.push(String(err)));

  await page.goto('/#/event/%/2026-01-01');

  // The shell renders. What it says about that event does not matter — there is no such
  // event, so an error where the event would be is the right answer — but there has to
  // be an application on the page to say it.
  await expect(page.getByRole('navigation').or(page.locator('.tabbar'))).toBeVisible({
    timeout: 15_000,
  });
  expect(errors.filter((e) => e.includes('URIError')), 'a URIError escaped the router').toEqual([]);

  // And the app still works from there: the calendar is reachable by the chrome the
  // shell drew, rather than only by reloading onto a URL that parses.
  await page.getByRole('button', { name: 'Month', exact: true }).click();
  await page.getByRole('button', { name: 'Today', exact: true }).click();
  await expect(page.getByText("Leo's dentist").first()).toBeVisible({ timeout: 15_000 });
});

// Opening an event should not resize the control next to it.
//
// The panel takes a column from the calendar, and the bar used to answer that by giving
// the view switch a line of its own and stretching it across the width — on every screen,
// however much room was left. The switch is a segmented pill with a background, so on a
// wide monitor that drew a full-width grey bar under the month, and the bar changed shape
// every time an event was opened or closed.
//
// Both halves are held here, because fixing one by itself gives the other back: the
// switch keeps the width of its two words, and the row stays on one line while there is
// room for one. 1400px leaves room even with the panel out; 1000px does not, and is here
// so that "one line" is not achieved by letting the month and year run under the switch.
test('opening the event panel does not resize or relayout the view switch', async ({ page }) => {
  await page.setViewportSize({ width: 1400, height: 900 });
  await page.goto('/#/month');
  await expect(page.locator('.month-grid')).toBeVisible({ timeout: 15_000 });

  const measure = () => page.evaluate(() => {
    const box = (sel) => document.querySelector(sel).getBoundingClientRect();
    const title = document.querySelector('.app-title');
    return {
      switchWidth: Math.round(box('.view-switch').width),
      barHeight: Math.round(box('.app-bar-row').height),
      panelOpen: document.querySelector('.app').classList.contains('is-panel-open'),
      // The bar may wrap, but never by hiding the one label that has to stay readable.
      titleClipped: title.scrollWidth > title.clientWidth + 1,
    };
  });

  const closed = await measure();
  expect(closed.panelOpen, 'the panel was already open before the test opened it').toBe(false);

  await page.locator('.chip').first().click();
  await expect(page.locator('.app.is-panel-open')).toBeVisible({ timeout: 15_000 });
  const open = await measure();

  expect(open.switchWidth, 'the view switch changed width when the panel opened')
    .toBe(closed.switchWidth);
  expect(open.barHeight, 'the bar took a second line although there was room for one')
    .toBe(closed.barHeight);
  expect(open.titleClipped, 'the month and year is clipped with the panel open').toBe(false);

  // Narrow enough that it genuinely cannot fit: it may wrap, and the switch still keeps
  // its own width rather than filling the line it wrapped onto.
  await page.setViewportSize({ width: 1000, height: 900 });
  await page.waitForTimeout(300);
  const narrow = await measure();
  expect(narrow.switchWidth, 'the view switch stretched across the line it wrapped onto')
    .toBe(closed.switchWidth);
  expect(narrow.titleClipped, 'the month and year is clipped on a narrow window').toBe(false);
});

test('a bookmarked weekly view lands on the month rather than on nothing', async ({ page }) => {
  // The weekly view was removed, and #/week is what anyone who had it open still has in
  // the address bar — a bookmark, an open tab, a shared link. It resolves through the
  // router's notFound handler, which is a general fallback rather than anything this
  // change added: nothing in the removal itself holds that route open, so the upgrade
  // path for those readers rests on a line in another file that no test names.
  await page.goto('/#/week');

  await expect(page.locator('.month-grid')).toBeVisible({ timeout: 15_000 });
  // Replaced rather than pushed, so Back leaves the app instead of returning to a view
  // that no longer exists and bouncing forward again.
  await expect.poll(() => page.evaluate(() => location.hash)).not.toContain('week');
});
