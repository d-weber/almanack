// Where the keyboard is while an overlay is open, and where it is put back afterwards.
//
// Every overlay in this app — the day sheet, the yes/no confirmation, and the scope
// question that asks *this / this and following / the whole series* — is one function,
// openOverlay() in web/js/ui.js. It announced itself as a modal dialog and behaved like
// none of one: Tab walked straight out of the panel and on into the month grid behind
// it, closing left the keyboard at the top of the document rather than on the control
// that had asked the question, and `role="dialog"` with no accessible name is announced
// as "dialog" and nothing else.
//
// These are browser tests because none of it exists anywhere else: focus order, the
// accessible name and `document.activeElement` are computed by the browser from the
// DOM, and the frontend takes no dependencies (CONVENTIONS §1), so there is no
// JavaScript unit runner to put them in.
//
// They assert what assistive technology would resolve — `getByRole('dialog', {name})`
// and `toBeFocused()` — rather than the attributes that happen to produce it, so they
// keep holding if the way the name or the trap is wired ever changes.
//
// Nothing here creates or deletes anything: the scope question is reached through the
// family the seed already has, and every one of these dialogs is dismissed rather than
// answered, so the calendar is left exactly as it was found.

import { test, expect } from '@playwright/test';

const PANEL = '.overlay-panel';

function isoDaysFromToday(days) {
  const d = new Date();
  d.setUTCDate(d.getUTCDate() + days);
  return d.toISOString().slice(0, 10);
}

/**
 * An occurrence of something that repeats, from the seeded family — the swimming
 * lesson every Tuesday, or the birthday. Any of them is enough: what makes the app ask
 * the scope question is that the occurrence belongs to a series at all.
 */
async function aRepeatingOccurrence(page) {
  const from = isoDaysFromToday(0);
  const to = isoDaysFromToday(60);
  const body = await (await page.request.get(`/api/v1/events?from=${from}&to=${to}`)).json();
  const occ = (body.occurrences || []).find((o) => o.recurrence_id != null);
  expect(occ, 'the seeded family must have a repeating event to ask the scope question about').toBeTruthy();
  return occ;
}

/** True while the keyboard is somewhere inside the open panel — the whole point. */
function focusIsInsideThePanel(page) {
  return page.evaluate((sel) => Boolean(document.activeElement && document.activeElement.closest(sel)), PANEL);
}

test('the scope question keeps Tab inside itself and hands the keyboard back', async ({ page }) => {
  const occ = await aRepeatingOccurrence(page);
  await page.goto(`/#/event/${occ.event_id}/${occ.occurrence_date}`);

  // Opened from the keyboard, because that is who this is for — and because it makes
  // the control that asked the question unambiguous when we come to check it got the
  // keyboard back.
  const deleteButton = page.getByRole('button', { name: /^Delete$/ });
  await expect(deleteButton).toBeVisible();
  await deleteButton.focus();
  await page.keyboard.press('Enter');

  // 1. It has a name. A screen reader used to announce this as "dialog", with the
  //    question itself — the one thing that makes the three answers mean anything —
  //    reachable only by browsing the page.
  const dialog = page.getByRole('dialog', { name: 'Which events should be deleted?' });
  await expect(dialog).toBeVisible();

  // 2. It takes the keyboard, and Tab cannot leave it. Backwards from the first answer
  //    lands on the last control rather than on whatever is behind the overlay, and
  //    forwards from there comes back round.
  const firstChoice = page.getByRole('button', { name: 'This event' });
  const cancel = page.getByRole('button', { name: 'Cancel' });
  await expect(firstChoice).toBeFocused();

  await page.keyboard.press('Shift+Tab');
  await expect(cancel).toBeFocused();

  await page.keyboard.press('Tab');
  await expect(firstChoice).toBeFocused();

  // Then more presses than the dialog has controls, in both directions: a trap that
  // only holds for one lap is not a trap, and the failure it is guarding against —
  // Tab number five landing on the month grid behind — is invisible in a screenshot.
  for (let i = 0; i < 8; i++) {
    await page.keyboard.press('Tab');
    expect(await focusIsInsideThePanel(page), `Tab #${i + 1} left the dialog`).toBe(true);
  }
  for (let i = 0; i < 8; i++) {
    await page.keyboard.press('Shift+Tab');
    expect(await focusIsInsideThePanel(page), `Shift+Tab #${i + 1} left the dialog`).toBe(true);
  }

  // 3. Escape still dismisses it, and the keyboard goes back to the button that asked —
  //    not to the top of the document, which is where answering a question used to
  //    leave you, with the whole event to tab through again to reach anything.
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
  await expect(deleteButton).toBeFocused();

  // The same for the answer buttons' own path out. Cancelling is a close like any
  // other, and the event is still here: this test asks the question three times and
  // never answers it.
  await page.keyboard.press('Enter');
  await expect(dialog).toBeVisible();
  await cancel.click();
  await expect(dialog).toHaveCount(0);
  await expect(deleteButton).toBeFocused();
});

test('the day sheet is named by the day it shows, and gives the day back', async ({ page }) => {
  await page.goto('/#/month');

  const cell = page.locator('.day-hit.is-today');
  await expect(cell).toHaveCount(1);
  await cell.focus();
  await page.keyboard.press('Enter');

  // The sheet's name is its heading — the date, and "Today" when it is today. Read off
  // the heading rather than rebuilt here, so this asserts the two agree in whichever
  // language and locale the suite is running in.
  const sheet = page.getByRole('dialog');
  await expect(sheet).toBeVisible();
  const heading = (await sheet.getByRole('heading').first().textContent()).trim();
  expect(heading).not.toBe('');
  await expect(sheet).toHaveAccessibleName(heading);

  for (let i = 0; i < 8; i++) {
    await page.keyboard.press('Tab');
    expect(await focusIsInsideThePanel(page), `Tab #${i + 1} left the day sheet`).toBe(true);
  }

  // Tapping the backdrop is the third way out of an overlay, and it must put the
  // keyboard back like the other two: the day cell that opened the sheet.
  await page.locator('.overlay').click({ position: { x: 5, y: 5 } });
  await expect(sheet).toHaveCount(0);
  await expect(cell).toBeFocused();
});

test('a panel that repaints itself does not lose the keyboard out of the back', async ({ page }) => {
  // The colour grid rebuilds every swatch each time one is picked, so the button the
  // keyboard was on is gone a moment later and the browser drops focus to the document
  // body — outside the panel, where the next Tab used to land on the screen behind.
  // Nothing is saved here: the dialog is dismissed, so no calendar is created.
  await page.goto('/#/calendars');
  await page.getByRole('button', { name: 'New calendar' }).click();

  const dialog = page.getByRole('dialog', { name: 'New calendar' });
  await expect(dialog).toBeVisible();

  const swatches = dialog.getByRole('radio');
  await expect(swatches.first()).toBeVisible();
  await swatches.nth(3).click();
  // The grid has redrawn once this holds: the swatch at that position is a new element,
  // and the one the mouse left the keyboard on no longer exists.
  await expect(swatches.nth(3)).toBeChecked();

  for (let i = 0; i < 4; i++) {
    await page.keyboard.press('Tab');
    expect(await focusIsInsideThePanel(page), `Tab #${i + 1} after a repaint left the dialog`).toBe(true);
  }

  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
});

// The two shapes below cannot be reached from any screen: every overlay the app builds
// today has a heading and several buttons. They are the shapes a focus trap gets wrong —
// nothing to cycle through, and one thing that is both the first and the last — and
// openOverlay is a shared widget that will be handed both eventually. So they are driven
// through the module directly, which is as close to a unit test as a project with no
// JavaScript test runner gets.
async function openBareOverlay(page, shape) {
  await page.evaluate(async (kind) => {
    const { openOverlay } = await import('/js/ui.js');
    if (kind === 'nothing-focusable') {
      const p = document.createElement('p');
      p.textContent = 'Nothing here can be focused.';
      openOverlay(p, { variant: 'dialog' });
      return;
    }
    const wrap = document.createElement('div');
    const title = document.createElement('h2');
    title.textContent = 'One way out';
    const only = document.createElement('button');
    only.type = 'button';
    only.textContent = 'The only button';
    wrap.append(title, only);
    openOverlay(wrap, { variant: 'dialog' });
  }, shape);
}

test('an overlay with nothing to focus is named, and still holds the keyboard', async ({ page }) => {
  await page.goto('/#/month');
  await openBareOverlay(page, 'nothing-focusable');

  // No heading to be labelled by, so it falls back to the catalogue's generic name.
  // "dialog", announced twice, is still an answer where nothing at all was one.
  const dialog = page.getByRole('dialog', { name: 'Dialog' });
  await expect(dialog).toBeVisible();

  // The panel itself takes the keyboard, and keeps it: letting Tab through would put
  // the reader in a page they cannot see past the backdrop, with no way back.
  for (let i = 0; i < 3; i++) {
    await page.keyboard.press('Tab');
    expect(await page.evaluate(() => document.activeElement.classList.contains('overlay-panel'))).toBe(true);
  }

  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
});

test('an overlay with one control tabs to itself rather than out', async ({ page }) => {
  await page.goto('/#/month');
  await openBareOverlay(page, 'one-button');

  const dialog = page.getByRole('dialog', { name: 'One way out' });
  const only = page.getByRole('button', { name: 'The only button' });
  await expect(dialog).toBeVisible();
  await expect(only).toBeFocused();

  // First and last are the same element here, which is the case a cycle written as
  // "on the last one, go to the first" gets right and one written as "go to the next
  // one" walks straight past.
  await page.keyboard.press('Tab');
  await expect(only).toBeFocused();
  await page.keyboard.press('Shift+Tab');
  await expect(only).toBeFocused();

  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
});
