// The other decoy. See device-timezone.spec.js for the whole of the reasoning.
//
// This one is the near-miss for logout.spec.js, which playwright.config.js routes into
// `chromium-logout` — a project that starts from no stored session at all, because
// signing out ends one server-side and the session every other spec shares is not its to
// destroy. An unanchored /logout\.spec\.js/ matches "family-logout.spec.js" as well, so
// this file would be run there: signed out, against a project it was never written for,
// and passing.

import { test, expect } from '@playwright/test';

test('is routed by the whole of its own filename, not by a pattern meant for another', () => {
  expect(
    test.info().project.name,
    'a filename pattern in playwright.config.js has stopped being anchored to a whole ' +
      'basename, and this file has been routed into another project by it (#63). Specs ' +
      'misfiled this way go on passing and say nothing, which is why this one exists.',
  ).toBe('chromium');
});
