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
| `internal/store` | Migrations and the newer-schema refusal, the CHECK constraints actually rejecting malformed events, override and series-split plumbing, accent-insensitive search, queue idempotency |
| `internal/webpush` | RFC 8291 Appendix A encryption vectors byte for byte, VAPID JWTs signed as raw R‖S, the TTL/Urgency/Topic header matrix, 404/410 → prune |
| `internal/notify` | The planner and the boot catch-up policy — including the "server was off for a week" case, which is as correctness-critical as the DST tests |
| `internal/holidays` | Easter from 1900 to 2100, and what happens when the family suppresses a holiday the law removed |
| `internal/httpapi` | Auth, CSRF, invites, scoped edits, and that a hostile event title comes back escaped |
| root `deps_test.go` | The dependency allowlist — the build fails if someone adds a module or an npm file |

Useful variants: `make race` (the scheduler shares state with HTTP handlers), `make cover`.

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

## Browser smoke tests (optional)

`make e2e` runs a small Playwright suite (login, create an event, see it in the month view, an
XSS-hostile event title, the service-worker update reload). It needs `npx`, which is not
installed here, so the target skips cleanly rather than failing. These tests are development-only
and are never part of the production build.

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
