# Conventions

Binding rules for anyone — human or agent — writing code in this repo. They exist because this
app must still build, run, and be patchable in 2040 by someone who has never seen it before.
When a rule here conflicts with habit, the rule wins. See [docs/architecture.md](docs/architecture.md)
for the reasoning.

## 1. Dependencies

- **Third-party Go modules are limited to an allowlist of two**: `modernc.org/sqlite` and
  `golang.org/x/crypto` (argon2id only — HKDF comes from stdlib `crypto/hkdf`).
  `internal/deps/deps_test.go` fails the build if `go.mod` gains anything else.
- **The frontend has zero dependencies.** No npm, no bundler, no framework, no CDN, no web fonts.
  The only build in this project is `go build`.
- Do not add a dependency to "save time". Write the 40 lines, or ask whether the feature is worth it.
- The `e2e/` Playwright suite is dev-only and never imported by the app or the production build.

## 2. Layout

```
cmd/almanack              the binary: subcommand dispatch, server wiring, seed, bootstrap, backup
internal/domain         core types shared by everything; depends on nothing but stdlib
internal/clock          Clock interface (real + fake); ALL time reads go through it
internal/config         env → Config
internal/store          SQLite: schema, migrations, every SQL query in the app
internal/recur          recurrence expansion (pure functions, no I/O)
internal/holidays       French public holidays (pure, computed)
internal/webpush        RFC 8030/8291/8292 sender (pure crypto + HTTP)
internal/imgproc        avatar decode/scale/encode (no image libraries)
internal/i18n           fr/en catalogs + French date formatting (server-side notification text)
internal/mailer         SMTP to localhost MTA, plus the dev file sink
internal/notify         planner, scheduler, catch-up policy, delivery
internal/httpapi        HTTP server, middleware, handlers, static/PWA serving
web/                    the PWA; web/embed.go compiles it into the binary
internal/deps           no code — the dependency policy, expressed as a test
e2e/                    dev-only browser smoke tests
```

**One package owns SQL: `internal/store`.** No other package writes SQL or touches `*sql.DB`.

## 3. Go style

- Standard library first, always. `net/http` + `ServeMux` patterns (`GET /api/v1/events/{id}`);
  no router, no middleware framework.
- `gofmt` (enforced by `make check`), `go vet` clean. No linter framework.
- Errors: wrap with context — `fmt.Errorf("load event %d: %w", id, err)`. Sentinel errors in
  `domain` (`domain.ErrNotFound`, `domain.ErrConflict`, …) so handlers can map them to status codes.
- Never `panic` in library code. `main` may fatal on startup misconfiguration, nowhere else.
- Logging: `log/slog` to stdout, structured (`slog.Info("msg", "key", val)`). No log files.
- Exported identifiers get doc comments. Comments explain *why*, never *what the next line does*.
- No `init()` functions except `go:embed` wiring. No global mutable state outside `main`.
- Concurrency stays boring: one scheduler goroutine and a ticker. If a change needs a mutex-heavy
  design, it is probably wrong for this app.

## 4. Time — the rules that prevent the classic calendar bugs

- **Never call `time.Now()` directly.** Take a `clock.Clock`. Tests use `clock.NewFake`,
  and dev mode can travel through time (`POST /dev/clock`), which is how reminders and digests
  get tested locally without waiting a day.
- **Timed events** are stored as UTC (`TEXT` ISO-8601 with `Z`). **All-day events** are stored as
  `domain.Date` (`TEXT` `YYYY-MM-DD`) — never as midnight instants. An all-day event has no
  timezone; treating it as one is the off-by-one-day bug.
- `end_date` on all-day events is **inclusive**.
- **All date bucketing** ("today", digest membership, occurrence identity) happens in the
  configured **family timezone**, on the server and in the browser alike.
- **Recurrence math uses WKST = Monday, always.** The per-user `week_start` preference is display
  only and must never reach `internal/recur`.
- Recurrence expands in family-tz wall-clock, then converts to UTC — so 16:30 stays 16:30 across
  a DST change.
- The frontend passes `timeZone: <family tz>` to every `Intl` call. The device timezone is never
  used: a phone in Lisbon still shows the Paris dentist at 16:30.

## 5. HTTP and security

- JSON REST under `/api/v1/`. Changes within `v1` are additive only.
- **Mutations are never GET.** Logout is POST. A single state-changing GET reopens CSRF.
- Every non-GET/HEAD/OPTIONS request must carry `X-Requested-With: almanack`; middleware rejects
  those that don't (this plus `SameSite=Lax` is the CSRF defense — there is no token).
- Every response carries `X-App-Version`; the client hard-refreshes on mismatch.
- Session cookie: `HttpOnly`, `SameSite=Lax`, `Secure` (except dev mode on localhost).
- Tokens (session, invite, password reset) are 256-bit random, stored **hashed** (SHA-256),
  never logged. Passwords are argon2id (RFC 9106: m=64 MiB, t=3, p=4).
- Authorization is checked per request: a user may only touch calendars they are a member of.
  Only the calendar creator may remove members. Otherwise permissions are flat by design.
- Error responses are `{"error":{"code":"...","message":"..."}}`. Never leak whether an email
  exists (password reset and login return the same shape either way).

## 6. Frontend

- ES2020 modules, no build step, no framework. Views render through the `h()` helper in `js/dom.js`.
- **`h()` escapes text by default. `innerHTML` with user data is forbidden.** Event handlers are
  attached with `addEventListener` only — the CSP ships without `unsafe-inline`, so an inline
  `onclick=` is a dead button, not a shortcut.
- CSP is `default-src 'self'`; do not weaken it. No inline `<script>`, no `eval`.
- The service worker must call `event.waitUntil(registration.showNotification(...))` on **every**
  push, including a generic fallback when a payload fails to parse — iOS revokes the subscription
  after roughly three pushes that display nothing.
- Cached API responses are a fallback only: refetch the visible range on focus/`visibilitychange`
  and after every mutation.
- All user-visible strings come from `internal/i18n/locales/{fr,en}.json` via `t()`. No hardcoded French or
  English in `.js`. The same catalogs are embedded server-side for notification text.

## 7. Testing

- `make check` (gofmt + vet + `go test ./...`) must be green before anything is considered done.
- Table-driven tests. Each policy row in the recurrence policy table in docs/architecture.md is a named test case.
- Tests never touch the network and never need a running server; they create a temp SQLite file.
- Time-dependent tests use `clock.NewFake` — never `time.Sleep` to wait for a scheduler.
- Crypto is verified against published vectors (RFC 8291 Appendix A), byte for byte.
- New behavior lands with its test in the same change. Bug fixes land with the failing case first.
- Every package has a coverage floor in `.github/scripts/check_coverage.py`, enforced by
  `make check`. Dropping below one, or adding a package with no entry, fails the build. Lowering
  a floor is allowed and must happen in the same commit as the change that needs it.

## 8. Migrations

- Numbered, embedded, immutable: `internal/store/migrations/000N_name.sql`. **Never edit an applied
  migration** — add the next one.
- **Expand/contract**: a migration must leave the previous binary able to run for one release
  (add columns/tables first; drop them a release later). This is what makes rollback a symlink flip.
- The binary refuses to start if the database schema is newer than it knows.
- **A migration is not done until it has been run against a real old database.** One that each
  released binary wrote is checked in as SQL text under `internal/store/testdata/`, and
  `TestUpgradeFromReleasedDatabase` replays it, opens it with the current binary and reads every
  row back through the store API. Never regenerate an old fixture against a newer schema — its
  value is that it is old, and the test says so when you try. A release that changes the schema
  adds a fixture of its own beside the others.
