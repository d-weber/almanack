// What a phone's calendar screen carries, and what it does not.
//
// The top bar used to hold Today, two month arrows, the view switch, search and add —
// six controls across the width of a phone, most of them at the far corner from the hand
// holding it. It now holds the month and the mode, and the things it lost became either
// a gesture (swipe to change month, tap the month to come back to today) or a control
// somewhere a thumb already is (the round add button on the tab bar's edge, and search
// on its own tab).
//
// This file sets its own viewport rather than relying on a project, because it is the
// only spec about the phone layout: the rest of the suite runs wide, where the bar keeps
// every control it ever had.

import { test, expect } from '@playwright/test';

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

test('the phone bar carries the month and the mode, and nothing else', async ({ page }) => {
  await page.goto('/#/month');
  await expect(page.locator('.month-grid')).toBeVisible({ timeout: 15_000 });

  // What is there.
  await expect(page.locator('.app-title')).toBeVisible();
  const modes = page.locator('.view-switch .segment');
  await expect(modes).toHaveCount(2);
  await expect(modes.nth(0)).toHaveText('Month');
  await expect(modes.nth(1)).toHaveText('Agenda');
  // The calendar filters stay on screen, which is what the freed room buys.
  await expect(page.locator('.cal-chips')).toBeVisible();

  // What is not.
  await expect(
    page.locator('.app-bar').getByRole('button', { name: /^today$/i }),
    'Today is a tap on the month title now, not a button in the bar',
  ).toHaveCount(0);
  await expect(
    page.locator('.app-bar .icon-btn'),
    'the month arrows and the search icon have left the phone bar',
  ).toHaveCount(0);
});

test('the round add button sits on the line between the calendar and the tabs', async ({ page }) => {
  await page.goto('/#/month');
  await expect(page.locator('.month-grid')).toBeVisible({ timeout: 15_000 });

  const geometry = await page.evaluate(() => {
    const box = (sel) => {
      const el = document.querySelector(sel);
      return el ? el.getBoundingClientRect() : null;
    };
    const fab = box('.fab');
    const tabs = box('.tabbar');
    if (!fab || !tabs) return null;
    return {
      fabCentreX: Math.round(fab.left + fab.width / 2),
      fabCentreY: Math.round(fab.top + fab.height / 2),
      tabsTop: Math.round(tabs.top),
      pageCentreX: Math.round(window.innerWidth / 2),
      round: fab.width === fab.height,
    };
  });
  expect(geometry, 'no round add button on the calendar screen').not.toBeNull();

  expect(geometry.round, 'the add button is not a circle').toBe(true);
  expect(geometry.fabCentreX, 'the add button is not horizontally centred').toBe(geometry.pageCentreX);
  // Straddling: its middle is the tab bar's top edge, half over the calendar and half
  // over the tabs. A pixel either way is rounding, not a layout that has drifted.
  expect(Math.abs(geometry.fabCentreY - geometry.tabsTop)).toBeLessThanOrEqual(1);

  // And it opens the editor, which is the whole of what it is for.
  await page.locator('.fab').click();
  await expect(page.locator('.editor')).toBeVisible({ timeout: 15_000 });
});

test('the month title goes back to today, and swiping changes month', async ({ page }) => {
  await page.goto('/#/month');
  await expect(page.locator('.month-grid')).toBeVisible({ timeout: 15_000 });
  const title = () => page.locator('.app-title').innerText();
  const start = await title();

  // A swipe is a touch that travels: dispatched rather than driven by the mouse,
  // because the gesture listens for touch alone and a mouse never produces it.
  const swipe = async (dx) => {
    await page.evaluate((delta) => {
      const el = document.querySelector('.month');
      const box = el.getBoundingClientRect();
      const y = box.top + box.height / 2;
      const x = box.left + box.width / 2;
      const touch = (target, cx) => new Touch({ identifier: 1, target, clientX: cx, clientY: y });
      el.dispatchEvent(new TouchEvent('touchstart', {
        bubbles: true, cancelable: true, touches: [touch(el, x)], changedTouches: [touch(el, x)],
      }));
      el.dispatchEvent(new TouchEvent('touchend', {
        bubbles: true, cancelable: true, touches: [], changedTouches: [touch(el, x + delta)],
      }));
    }, dx);
    await page.waitForTimeout(400);
  };

  await swipe(-120);
  const next = await title();
  expect(next, 'swiping left did not move to the next month').not.toBe(start);

  await swipe(120);
  expect(await title(), 'swiping right did not come back').toBe(start);

  // A tap that drifts a little is a tap, not a swipe — otherwise opening a day would
  // page the month out from under the finger.
  await swipe(-20);
  expect(await title(), 'a 20px drag was taken for a swipe').toBe(start);

  // Somewhere else entirely, then back via the title.
  await swipe(-120);
  await swipe(-120);
  expect(await title()).not.toBe(start);
  await page.locator('.app-title-btn').click();
  await page.waitForTimeout(400);
  expect(await title(), 'tapping the month title did not come back to today').toBe(start);
});

test('the theme is settable on a phone, where the sidebar that holds it does not exist', async ({ page }) => {
  await page.goto('/#/settings');
  await expect(page.getByRole('tab', { name: 'Dark' })).toBeVisible({ timeout: 15_000 });

  await page.getByRole('tab', { name: 'Dark' }).click();
  await expect
    .poll(() => page.evaluate(() => document.documentElement.getAttribute('data-theme')))
    .toBe('dark');

  // Put it back, or every spec after this one runs in the dark — and the setting is
  // stored per device rather than per account, so it would outlive this test.
  await page.getByRole('tab', { name: 'System' }).click();
  await expect
    .poll(() => page.evaluate(() => document.documentElement.getAttribute('data-theme')))
    .toBe(null);
});
