// Development-only browser smoke tests.
//
// These are never part of the production build: no Go code imports them, `make build`
// ignores them, and `make e2e` skips silently when npx is unavailable. They exist to
// catch the handful of things unit tests structurally cannot — that the app actually
// renders, that the CSP does not break the UI, and that a hostile event title stays
// inert in a real DOM.
//
// Run against a seeded dev server:  make seed && make dev   (then, elsewhere) make e2e
// They also run in CI, which seeds a family and starts a server first.

const STATE_PATH = '.auth/state.json';

// The three files that are routed by name rather than by the default pattern, each
// anchored to the whole of its basename.
//
// Playwright tests these against the absolute path (and again against it with forward
// slashes, on Windows), so an unanchored /timezone\.spec\.js/ matches anywhere in it. A
// later device-timezone.spec.js would therefore be dropped from `chromium` and picked
// up by `chromium-lisbon` instead — a different device timezone, a different session —
// and nothing would report the misfiling: the spec still runs, still passes, and says
// nothing about the project it was meant to be in. That happened while the spec for #58
// was being written, and was worked around by calling the file unknown-tz.spec.js. A
// filename should not have to dodge a pattern. `(^|/)` and `$` make the basename the
// whole of the match, so only these three files can ever be these three files.
const TIMEZONE_SPEC = /(^|\/)timezone\.spec\.js$/;
const LOGOUT_SPEC = /(^|\/)logout\.spec\.js$/;
const AUTH_SETUP = /(^|\/)auth\.setup\.js$/;

export default {
  testDir: '.',
  timeout: 30_000,
  expect: { timeout: 5_000 },
  fullyParallel: false,
  // Every test talks to one server backed by one SQLite file and one seeded family, so
  // they are not independent: run them one at a time rather than letting two workers
  // interleave writes and sessions.
  workers: 1,
  // One retry, and it buys exactly one thing: chrome-headless-shell segfaults mid-run,
  // and the test that reports it is the *next* one to ask for a context — an unrelated
  // test failed by a browser that died during its predecessor (#80). Failing the run on
  // that says nothing true about the code.
  //
  // Everything a retry would otherwise hide is taken back by ./crash-only-retry.js, which
  // fails the run again if any test passed on a retry for a reason that was not the crash.
  // That keeps the property this suite is built on — an intermittent failure turns CI red
  // and names itself — while dropping the one failure that never meant anything.
  //
  // This used to be 0 here while CI passed --retries=1 on the command line, so the suite
  // behaved one way on a laptop and another way in CI, and the setting each reader found
  // depended on which file they opened. The flag is gone from the workflow; this is now
  // the only place it is decided.
  retries: 1,
  reporter: [['list'], ['./crash-only-retry.js']],
  use: {
    // Deliberately outside the ALMANACK_ namespace: the server treats an unknown
    // ALMANACK_* variable as a startup error, so a test-only setting that squatted
    // in there would stop `make dev` from starting in any shell that exported it.
    baseURL: process.env.E2E_BASE_URL || 'http://localhost:8080',
    locale: 'en-GB',
    timezoneId: 'Europe/Paris',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    // No service worker, unless a spec asks for one.
    //
    // web/sw.js calls skipWaiting() and clients.claim(), so it takes control partway
    // through a visit rather than on the next one, and its fetch handler answers /api/.
    // The requests it makes are its own rather than the page's, and page.route never
    // sees them — so every spec that answers an API path for itself was racing the
    // worker for control of that request and losing about one run in five (#79). When it
    // lost, the route did not fire and the spec failed over something it was not
    // testing: intermittent, green on the re-run, and expensive to trace back.
    //
    // The alternative was to write the constraint down beside page.route and leave the
    // default alone. A note only reaches whoever reads it before writing the spec, and
    // the failure it prevents points nowhere near it — this suite has twice paid for a
    // failure that pointed away from its cause (#52, #66). Blocked by default, the trap
    // is not there to fall into; the two files that are *about* the worker turn it back
    // on in one line each, in the file whose subject it is, and a worker test written
    // without that line fails the same way every time. A deterministic failure is the
    // cheap kind.
    serviceWorkers: 'block',
  },
  projects: [
    // Signs in through the real form and saves the session for everything below. See
    // auth.setup.js for why this is not done once per test.
    { name: 'setup', testMatch: AUTH_SETUP },
    {
      name: 'chromium',
      use: { browserName: 'chromium', storageState: STATE_PATH },
      dependencies: ['setup'],
      // timezone.spec.js asserts the device zone is Lisbon, which is only true in the
      // project below. Running it here failed on an assertion that was never about the
      // application. logout.spec.js needs a session it is allowed to destroy, which the
      // shared one is not.
      testIgnore: [TIMEZONE_SPEC, LOGOUT_SPEC],
    },
    // A second timezone catches the bug this project cares most about: a device in
    // Lisbon must still show a Paris event at its Paris time.
    {
      name: 'chromium-lisbon',
      use: {
        browserName: 'chromium',
        timezoneId: 'Europe/Lisbon',
        storageState: STATE_PATH,
      },
      dependencies: ['setup'],
      testMatch: TIMEZONE_SPEC,
    },
    // Signing out ends a session server-side, and the one above is shared by every other
    // spec. So this project starts from no stored session at all and signs in as somebody
    // else: what it ends belongs to it alone. See logout.spec.js.
    {
      name: 'chromium-logout',
      use: { browserName: 'chromium', storageState: { cookies: [], origins: [] } },
      testMatch: LOGOUT_SPEC,
    },
  ],
};
