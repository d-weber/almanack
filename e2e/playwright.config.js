// Development-only browser smoke tests.
//
// These are never part of the production build: no Go code imports them, `make build`
// ignores them, and `make e2e` skips silently when npx is unavailable. They exist to
// catch the handful of things unit tests structurally cannot — that the app actually
// renders, that the CSP does not break the UI, and that a hostile event title stays
// inert in a real DOM.
//
// Run against a seeded dev server:  make seed && make dev   (then, elsewhere) make e2e

export default {
  testDir: '.',
  timeout: 30_000,
  expect: { timeout: 5_000 },
  fullyParallel: false,
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
    { name: 'chromium', use: { browserName: 'chromium' } },
    // A second timezone catches the bug this project cares most about: a device in
    // Lisbon must still show a Paris event at its Paris time.
    {
      name: 'chromium-lisbon',
      use: { browserName: 'chromium', timezoneId: 'Europe/Lisbon' },
      testMatch: /timezone\.spec\.js/,
    },
  ],
};
