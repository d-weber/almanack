# Testing everything locally

Everything in this project runs on a laptop with no server, no mail account, no domain and
no push service. This document is the tour.

## Prerequisites

Go (1.25+). On this machine it lives at `~/.local/go/bin/go`; the Makefile finds it there
automatically, so `make` works whether or not it is on your `PATH`. Add it permanently with:

```sh
echo 'export PATH=$HOME/.local/go/bin:$PATH' >> ~/.zshrc
```

Nothing else is required. There is no npm, no Docker, no database server to install —
SQLite is a file and the frontend has no toolchain.

## The two commands

```sh
make check     # gofmt + go vet + every test
make seed && make dev
```

`make dev` serves <http://localhost:8080>. Sign in as **mum@example.org** / **password**
(the demo family the seeder creates, all with the same password). Gran reads the app in
French, so signing in as **gran@example.org** is the quickest way to check a translation.

`make seed` wipes `devdata/` and rebuilds it, so run it whenever you want a clean slate.

## What the tests cover

`make check` runs everything; these are the parts worth knowing about.

| Package | What its tests prove |
|---|---|
| `internal/recur` | Every policy row in the recurrence policy table in docs/architecture.md: the 31st skips short months, 29 February clamps, `UNTIL` is inclusive, every-2-weeks does not drift across a year, "2nd Tuesday" and "last Friday", Monday-anchored week parity |
| `internal/store` | Migrations and the newer-schema refusal, the CHECK constraints actually rejecting malformed events, override and series-split plumbing, accent-insensitive search, queue idempotency. Also that **an existing calendar survives an upgrade**: a database the v0.2.0 binary really wrote, checked in as SQL text under `internal/store/testdata/`, is replayed into a fresh file, opened by the current binary, and read back through the store API with every row still there and still meaning the same thing |
| `internal/webpush` | RFC 8291 Appendix A encryption vectors byte for byte, VAPID JWTs signed as raw R‖S, the TTL/Urgency/Topic header matrix, 404/410 → prune |
| `internal/notify` | The planner and the boot catch-up policy — including the "server was off for a week" case, which is as correctness-critical as the DST tests |
| `internal/holidays` | Easter from 1900 to 2100, and what happens when the family suppresses a holiday the law removed |
| `internal/httpapi` | Auth, CSRF, invites, scoped edits, and that a hostile event title comes back escaped |
| `internal/auth` | That the argon2id parameters really are the RFC 9106 ones — a silent weakening of the memory or time cost fails the build — and that tokens are stored as SHA-256 hashes and never in the clear |
| `internal/config` | The strictness promised in 0.2.0: an unknown key or an unparseable value is an error that names the setting. Also that `almanack.conf.example` and the parser agree on the set of keys, in both directions, so the example cannot drift out of sync with the code |
| `internal/mailer` | That a newline in a subject cannot inject a header — the 0.2.0 bug — accented text survives encoding, and an MTA failure is returned rather than swallowed |
| `cmd/almanack` | That a snapshot is a real database with the rows still in it, that `--prune` with zero retention deletes nothing (it once deleted everything, including the snapshot it had just taken), and that `bootstrap` refuses to run once an account exists |
| root `deps_test.go` | The dependency allowlist — the build fails if someone adds a module or an npm file |

Useful variants: `make race` (the scheduler shares state with HTTP handlers), `make cover`.

### The coverage floors

`make check` ends with `make cover-check`, which fails if any package has dropped below the
floor recorded for it in `.github/scripts/check_coverage.py`. Coverage that is only ever
*reported* rots quietly, and the packages that slide are the ones being changed most.

Two rules, and the second matters more: a package may not fall below its floor, and a package
must be **in the file at all** — a new package with no entry fails even if it is well tested,
because otherwise one can be added with no tests and nothing notices. Adding uncovered code
therefore means writing the test, or lowering the floor in the same commit, which is a visible
decision in a diff rather than a silent drift.

The floors are set at what the suite achieves today, so they answer "has this got worse",
not "is this good enough". Raise them when you raise the coverage.

## Testing notifications without a push service or a mail server

This is the part that normally forces you onto a real server. It doesn't here.

**Email** — dev mode never sends anything. Every message is written as an `.eml` file to
`devdata/mail/` and listed at <http://localhost:8080/dev/mail>. Password resets, reminders and
digests all land there, fully rendered.

**Push** — real Web Push needs a browser push service, but the pipeline in front of it does not.
Dev mode records every notification the server decides to send at
<http://localhost:8080/dev/notifications>, whether or not a device is subscribed. That is where
you check that the right person is being notified, at the right time, in the right language.

Actual end-to-end push *does* work locally in Chrome or Firefox: `http://localhost` counts as a
secure context, so service workers and `PushManager.subscribe()` function normally (this needs
an internet connection, since the subscription lives with Mozilla's or Google's push service).
Safari and iOS are the exception — those need HTTPS and a home-screen install, which is what the
real deployment is for.

**Time travel** — reminders and digests are scheduled hours or days out, and nobody wants to wait
until 07:30 tomorrow to test the digest. Dev mode exposes a controllable clock:

```sh
curl -X POST localhost:8080/dev/clock -H 'X-Requested-With: almanack' -d '{"advance":"26h"}'
curl -X POST localhost:8080/dev/tick  -H 'X-Requested-With: almanack'
```

Advance the clock, force a planner and scheduler pass, then look at `/dev/notifications` and
`/dev/mail` to see exactly what the family would have received. The same mechanism is what the
outage-recovery test uses: set the clock forward a week, restart, and assert that the catch-up
policy delivers the still-relevant reminders and skips the ones whose events have passed.

## Testing the backup and restore path

```sh
make backup                       # writes a verified snapshot into devdata/backups
```

The subcommand does what the server does nightly: `VACUUM INTO` a temporary file, run
`PRAGMA integrity_check` on the *output*, fsync, then atomically rename. A failed integrity
check exits non-zero — which on the real server sends the owner an email.

To rehearse a restore: stop the server, copy a snapshot over `devdata/almanack.db`, start again,
and confirm your events are there. That is the whole procedure, and it is deliberately the same
one in `docs/RESTORE.md`.

## Browser smoke tests

`make e2e` runs a small Playwright suite (login, create an event, see it in the month view, an
XSS-hostile event title, the service-worker update reload) against a dev server you have already
started with `make dev`. Locally it needs `npx`, and the target skips cleanly rather than failing
when that is absent.

Every spec that creates an event deletes it again in a `finally`, so the suite can be run twice
against one database and leaves the seeded family as it found it. A run that is interrupted can
still leave a fixture behind, and the first thing the suite does is look for one and stop with the
answer — `make seed` — rather than letting three unrelated-looking tests fail over an extra event.
Fixtures are deleted through the API, never through the UI: deleting an event in the browser is a
flow of its own, and a test about something else is the wrong place to exercise it.

What the suite spends on the server it gives back too. It signs in twice a run — once through the
real login form, and once more in the sign-out project, which needs a session of its own to
destroy — and the login endpoint's bucket holds eight attempts per address, refills at one per
twenty seconds and does not empty between runs, so five runs inside a minute used to end at the
password box with a failure that pointed at the month view. The setup step now empties the buckets
before it spends anything (`POST /dev/ratelimits/reset`, dev mode only, with a button for it on
the dev dashboard — which is also the answer when you have just mistyped your own password eight
times). The limits are the production ones in dev as well: what dev mode adds is the emptying.

**These run in CI**, in their own job, which seeds a demo family and starts a server first. They
are not optional there, and that is deliberate: three of the bugs 0.2.0 fixed were reachable only
through a real browser — an upload the app's own CSP forbade, an invite link that never opened the
join screen, and a new build that could not reach an open tab. None of them could have failed a Go
test. npm appears in that job and nowhere else; the `e2e` directory is the one place the dependency
policy allows it, nothing it installs is committed, and none of it reaches the binary.

## Layout of `devdata/`

```
devdata/almanack.db        the SQLite database (plus -wal and -shm while running)
devdata/mail/            .eml files the dev mailer wrote
devdata/backups/         snapshots from `make backup`
```

Everything under `devdata/` is disposable and git-ignored. `make clean` removes it along with
build output.

## Cutting a release

Releases are built by `.github/workflows/release.yml` when a `v*` tag is pushed. There is
nothing to run by hand and no artefact to upload yourself.

```sh
# 1. Move the Unreleased section of CHANGELOG.md under a new "## [0.2.0] — YYYY-MM-DD"
#    heading, and add the two link definitions at the foot of the file.
# 2. Commit that, then:
git tag -a v0.2.0 -m "Almanack 0.2.0"
git push origin main --follow-tags
```

The workflow re-runs the test suite against the tagged commit, cross-compiles the five
binaries in `RELEASE_TARGETS` (see the Makefile), checks each one really is the
architecture its name claims and that the version was stamped into it, verifies the
checksums, and publishes a release whose notes are that version's changelog section.

**The changelog is the release notes.** `.github/scripts/release_notes.py` extracts them,
and exits non-zero when the tag has no section — so tagging a version you forgot to write
down fails before anything is published, rather than shipping an empty release page.

To rehearse without releasing, run the workflow from the Actions tab with no tag. It does
every step, proves it is permitted to publish by creating and immediately deleting a draft,
and leaves the binaries on the run as an artefact instead of creating a release.

`make build-all` produces exactly the same set of binaries locally, which is the quickest
way to check that a change has not broken a platform nobody here runs.
