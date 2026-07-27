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
    baseURL: process.env.ALMANACK_URL || 'http://localhost:8080',
    locale: 'en-GB',
    timezoneId: 'Europe/Paris',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    // Signs in through the real form and saves the session for everything below. See
    // auth.setup.js for why this is not done once per test.
    { name: 'setup', testMatch: /auth\.setup\.js/ },
    {
      name: 'chromium',
      use: { browserName: 'chromium', storageState: STATE_PATH },
      dependencies: ['setup'],
      // timezone.spec.js asserts the device zone is Lisbon, which is only true in the
      // project below. Running it here failed on an assertion that was never about the
      // application.
      testIgnore: /timezone\.spec\.js/,
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
      testMatch: /timezone\.spec\.js/,
    },
  ],
};
