// A screen releases what it holds when another screen replaces it.
//
// The agenda and the activity feed both page by watching a sentinel at the foot of
// their list with an IntersectionObserver. Each disconnects it when the list is
// complete; neither could disconnect it when the reader simply left, because a view is
// a function returning a node and nothing tells it the node was dropped. So app.js
// calls a `cleanup` property off that node when it mounts the next screen.
//
// What is asserted here, and what is not:
//
//   - asserted: the observer is disconnected when the screen is replaced, and going
//     back to the screen builds one observer rather than a second beside the first.
//     That is exactly what the fix does.
//   - not asserted: that the observer is then collected. No browser exposes that, and
//     there is no user-visible symptom to assert instead — a detached sentinel cannot
//     intersect, so a leaked observer never fires again. This is untidiness, not a
//     broken screen, and a test claiming more than that would be claiming it falsely.
//
// The counting is installed by the test, before any application code runs, rather than
// being exported by the app. Production code whose only purpose is to be watched by a
// test costs more than the test is worth.

import { test, expect } from '@playwright/test';

/**
 * Count IntersectionObservers as they are made and disconnected, and hang the one
 * request the screen under test needs in order to finish paging.
 *
 * The hang is what makes the assertion mean anything. Both screens disconnect as soon
 * as their list is complete, and the demo seed completes either of them in a single
 * page — so without it the observer would already be gone by the time the test
 * navigated away, and the test would pass just as happily against the bug. A request
 * that never answers holds the screen where the bug lives: mid-page, still observing.
 */
function instrument(hangPath) {
  const Native = window.IntersectionObserver;
  window.__io = { made: 0, live: 0 };
  window.IntersectionObserver = class extends Native {
    constructor(...args) {
      super(...args);
      this.__counted = true;
      window.__io.made += 1;
      window.__io.live += 1;
    }

    disconnect() {
      if (this.__counted) {
        this.__counted = false;
        window.__io.live -= 1;
      }
      return super.disconnect();
    }
  };

  const nativeFetch = window.fetch.bind(window);
  window.fetch = (input, init) => {
    const url = typeof input === 'string' ? input : String((input && input.url) || '');
    if (url.includes(hangPath)) return new Promise(() => { /* never answers */ });
    return nativeFetch(input, init);
  };
}

// -1 while the counter does not exist yet, so the first poll of a test waits for the
// document to have run the script above rather than failing on it.
const live = (page) => page.evaluate(() => (window.__io ? window.__io.live : -1));
const made = (page) => page.evaluate(() => (window.__io ? window.__io.made : -1));

// The tabs are matched exactly. Playwright matches an accessible name loosely by
// default, and the settings screen — where each of these tests goes and comes back from
// — also has a "New calendar" button, which a loose "Calendar" would find as well.
const tab = (page, name) => page.getByRole('button', { name, exact: true });

test('the agenda disconnects its paging observer when another screen replaces it', async ({ page }) => {
  await page.addInitScript(instrument, '/api/v1/events');
  await page.goto('/#/agenda');

  // The observer exists as soon as the screen is built, before its first page answers.
  // The wait is long only because it also covers the boot: config, catalogue and /me.
  await expect.poll(() => live(page), { timeout: 15_000 }).toBe(1);

  await tab(page, 'Settings').click();
  await expect.poll(() => live(page)).toBe(0);

  // Coming back builds a new one — and only one, with no survivor beside it. The count
  // of observers ever made is read rather than pinned to two, because the app also
  // repaints a calendar screen when the window regains focus.
  const before = await made(page);
  await tab(page, 'Calendar').click();
  await expect.poll(() => live(page)).toBe(1);
  expect(await made(page)).toBeGreaterThan(before);

  await tab(page, 'Settings').click();
  await expect.poll(() => live(page)).toBe(0);
});

test('the activity feed disconnects its paging observer when another screen replaces it', async ({ page }) => {
  await page.addInitScript(instrument, '/api/v1/activity');
  await page.goto('/#/activity');

  await expect.poll(() => live(page), { timeout: 15_000 }).toBe(1);

  await tab(page, 'Settings').click();
  await expect.poll(() => live(page)).toBe(0);

  const before = await made(page);
  await tab(page, 'Activity').click();
  await expect.poll(() => live(page)).toBe(1);
  expect(await made(page)).toBeGreaterThan(before);

  await tab(page, 'Settings').click();
  await expect.poll(() => live(page)).toBe(0);
});
