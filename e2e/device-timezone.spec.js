// A decoy. Its subject is the test runner, not the application.
//
// playwright.config.js routes three files by name: timezone.spec.js into the Lisbon
// project, logout.spec.js into the one with no saved session, auth.setup.js into setup.
// Everything else falls to the default pattern and runs in `chromium`. Those three
// patterns were once unanchored, and Playwright matches them against the whole absolute
// path — so a filename that merely *ends* in the same characters matched too. This one
// does: "device-timezone.spec.js" contains "timezone.spec.js". It was therefore taken
// out of `chromium`, where it belongs, and run in `chromium-lisbon` instead — a different
// device timezone and a different session — and nothing reported it. The file still ran
// and still passed. That is the whole difficulty with a guard that fails to guard: it
// looks exactly like one that works.
//
// #63 anchored the patterns with `(^|/)` and `$`, and shipped no test, having proved the
// point with two throwaway files it did not commit. This is one of them, kept. It asserts
// nothing about the calendar on purpose — the name is the test, and the assertion is
// simply which project the runner decided this name belonged to.

import { test, expect } from '@playwright/test';

test('is routed by the whole of its own filename, not by a pattern meant for another', () => {
  expect(
    test.info().project.name,
    'a filename pattern in playwright.config.js has stopped being anchored to a whole ' +
      'basename, and this file has been routed into another project by it (#63). Specs ' +
      'misfiled this way go on passing and say nothing, which is why this one exists.',
  ).toBe('chromium');
});
