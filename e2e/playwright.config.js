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
  retries: 0,
  reporter: [['list']],
  use: {
    // Deliberately outside the ALMANACK_ namespace: the server treats an unknown
    // ALMANACK_* variable as a startup error, so a test-only setting that squatted
    // in there would stop `make dev` from starting in any shell that exported it.
    baseURL: process.env.E2E_BASE_URL || 'http://localhost:8080',
    locale: 'en-GB',
    timezoneId: 'Europe/Paris',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
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
