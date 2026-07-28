// A link on an event must never be able to execute.
//
// An event carries a user-typed URL, and the detail screen draws it as an anchor:
// occ.url → views/event.js → metaLine() → h('a', {href}) → safeHref(). That last hop is
// where this app promises the value is made safe (CONVENTIONS §6: the DOM layer is the
// guardrail), and it was not keeping the promise. safeHref tested the scheme with
// /^[a-z][a-z0-9+.-]*:/, which a control character inside the scheme does not match, so
// 'java<TAB>script:alert(1)' fell past the check as "some kind of path" and was handed
// to setAttribute verbatim — where the URL parser removes the tab and hands back
// javascript:. The same went for a leading NUL, which JavaScript's trim() does not
// remove and the URL parser does.
//
// The payloads arrive by rewriting the detail response rather than by storing them,
// because the server refuses them at the door: a link must begin with http:// or
// https:// (internal/httpapi/events.go), so no fixture created through the API can
// carry one. That refusal is the second layer, and it is worth having, but it is not
// what this file is about. The question here is the first layer — whether the renderer
// is safe on its own, given a hostile value from anywhere at all — and the only honest
// way to ask it is to hand the renderer a hostile value.
//
// The rewritten response reaches the page because the suite blocks service workers
// (playwright.config.js, #79): a worker that has claimed the page serves the fetch
// itself, and page.route never sees it.
//
// This is a browser test because the sink is a browser: what makes the tab dangerous is
// that setAttribute('href', …) removes it at URL-parse time, and nothing outside a real
// DOM does that. The frontend takes no dependencies (CONVENTIONS §1), so there is no
// JavaScript unit runner to put it in either.

import { test, expect } from '@playwright/test';

const HEADERS = { 'X-Requested-With': 'almanack' };
const DATE = '2026-08-12';

// Each of these must leave the anchor with no href at all. The comment on each says what
// the browser would have made of it had it reached setAttribute.
const HOSTILE = [
  ['a tab inside the scheme', 'java\tscript:alert(1)'], // tab is removed anywhere in a URL
  ['a newline inside the scheme', 'java\nscript:alert(1)'], // so is LF
  ['a carriage return inside the scheme', 'java\rscript:alert(1)'], // and CR
  ['a leading NUL', '\x00javascript:alert(1)'], // leading C0 controls are stripped
  ['a leading SOH', '\x01javascript:alert(1)'], // NUL is not a special case
  ['a leading tab', '\tjavascript:alert(1)'], // trim() already caught this one
  ['leading spaces', '   javascript:alert(1)'], // and this one — neither may regress
  ['a data: URL behind a NUL', '\x00data:text/html,<h1>hello'], // not only javascript:
];

// And these must still be links: stripping the control characters must not cost the
// family a working one.
const BENIGN = [
  ['a plain https link', 'https://example.org/ok', 'https://example.org/ok'],
  ['whitespace around a link', ' \r\n https://example.org/ok \t', 'https://example.org/ok'],
];

/** Create a timed event with a harmless link, and hand back its id. */
async function createEvent(page) {
  const me = await (await page.request.get('/api/v1/me')).json();
  const calendar = me.calendars[0];
  const response = await page.request.post('/api/v1/events', {
    headers: HEADERS,
    data: {
      calendar_id: calendar.id,
      label_id: calendar.labels[0].id,
      title: 'Link under test',
      all_day: true,
      start_date: DATE,
      end_date: DATE,
      url: 'https://example.org/placeholder',
      participants: [],
    },
  });
  expect(response.status(), await response.text()).toBe(201);
  return (await response.json()).event.id;
}

async function deleteEvent(page, id) {
  await page.request.delete(`/api/v1/events/${id}`, { headers: HEADERS });
}

/**
 * Open the event's detail screen with `url` substituted into the response, and report
 * what the anchor ended up holding: the attribute as written, and the href the browser
 * resolved it to — which is the one that decides whether a click executes anything.
 */
async function renderedLink(page, id, url, nth) {
  const pattern = `**/api/v1/events/${id}?*`;
  await page.route(pattern, async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    body.occurrence.url = url;
    await route.fulfill({ response, json: body });
  });
  try {
    // The query string is what makes each of these a fresh document load. Going to a URL
    // that differs from the last one only in its hash is a same-document navigation: the
    // router would not run, nothing would be fetched, and every payload after the first
    // would be measuring the first one's anchor.
    await page.goto(`/?payload=${nth}#/event/${id}/${DATE}`);
    const link = page.locator('a.meta-link');
    await expect(link).toHaveCount(1);
    return await link.evaluate((a) => ({ attribute: a.getAttribute('href'), resolved: a.href }));
  } finally {
    await page.unroute(pattern);
  }
}

test('a link whose scheme hides a control character is not made into an href', async ({ page }) => {
  await page.goto('/');
  const id = await createEvent(page);

  try {
    // Softly, so that a failure names every payload that got through rather than only
    // the first one: which of these are let past is the whole diagnosis.
    for (const [nth, [what, url]] of HOSTILE.entries()) {
      const link = await renderedLink(page, id, url, nth);
      expect.soft(link.attribute, `${what}: ${JSON.stringify(url)} was written into href`).toBeNull();
      expect.soft(link.resolved, `${what}: ${JSON.stringify(url)} resolved to an executable URL`).toBe('');
    }
  } finally {
    await deleteEvent(page, id);
  }
});

test('an ordinary link still survives having its control characters removed', async ({ page }) => {
  await page.goto('/');
  const id = await createEvent(page);

  try {
    for (const [nth, [what, url, expected]] of BENIGN.entries()) {
      const link = await renderedLink(page, id, url, nth);
      expect.soft(link.attribute, `${what}: ${JSON.stringify(url)} lost its href`).toBe(expected);
    }
  } finally {
    await deleteEvent(page, id);
  }
});
