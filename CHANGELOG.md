# Changelog

Notable changes to this project. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

_Nothing yet._

## [0.2.0] — 2026-07-27

Everything an adversarial review of 0.1.0 found and fixed, an English demo
seed, and binaries you no longer have to build yourself.

### Added

- **Released binaries.** Pushing a `v*` tag now builds and publishes linux amd64, arm64
  and armv7, plus macOS amd64 and arm64, with a `SHA256SUMS` file and the version's
  changelog section as the release notes. Running it is no longer the only way to get it.

### Changed

- **The demo seed is in English.** `make seed` now creates Mum, Dad, Leo and Gran with
  the calendars Family, Parents and Kids' activities, and signs in with
  `mum@example.org` / `password`. Gran still reads the app in French, because a family
  that does not share one language is the case this was built for.
- **A new calendar's ten labels are now named after their own colours** — Emerald
  green, Lagoon teal, Sky blue, Warm taupe, Midnight black, Poppy red, Raspberry pink,
  Sunset coral, Golden amber, Soft lilac — on the palette TimeTree uses. A label starts
  as a colour and nothing else; what it means is what a group types in. That is also why
  they are not translated: guessing a meaning would freeze every label in the language of
  whoever created the calendar. Calendars that already exist keep the labels they have.

### Fixed

Findings from an adversarial review of the built system. Each was reproduced before
being fixed, and each has a regression test.

- **A reminder for an imminent event was never sent.** Adding an appointment shortly
  before it starts left its reminder slot already in the past, and the planner refused
  to queue it — so no notification was produced, then or ever.
- **A rejected "this and following" edit destroyed the rest of the series.** The
  original series was truncated before the replacement had been validated.
- **"This and following" did not move the repeat pattern.** Moving a weekly event to
  another weekday, or a monthly one to another day of the month, left the series on the
  old day and the moved occurrence did not exist at all.
- **An edited occurrence could become unreachable** when a split changed the pattern.
- **The outbox is now reconciled on every planning pass.** Changing a reminder, muting a
  calendar, switching the daily digest off or moving its time all used to leave the old
  notification queued, so it fired anyway.
- **The daily operations heartbeat now exists.** `ALMANACK_OWNER_EMAIL` and
  `ALMANACK_HEARTBEAT_TIME` were required settings that nothing read.
- **`/healthz` now reports backup age and result.** The metadata it reads was never
  written, so a server that had never once been backed up reported itself healthy.
- **`almanack backup --prune` no longer deletes every snapshot** when all retention
  settings are zero — including the one it had just written and reported as a success.
  Snapshots are also verified for referential integrity, named to the second, and refuse
  to run against a missing database.
- **Avatars are no longer readable across groups.** The route was the one id-scoped
  endpoint without a membership check.
- **A newline in an event title no longer kills the reminder email** for every
  participant.
- **Cancelling a date that is not an occurrence is refused** instead of writing an
  exception that would spring to life later.
- Configuration is now strict: an unparseable value or an unknown `ALMANACK_*` key is a
  startup error naming the problem, rather than a silent fall back to the default.
- The Go version floor, `make e2e`, `make cover`, the dependency allowlist (which was
  checking an empty list), and several broken documentation links were all corrected.

Then a browser pass, which found three things nothing server-side could have:

- **Avatar and calendar-picture uploads never worked.** The client resized images
  through a `blob:` URL, which the app's own Content-Security-Policy forbids, so
  every upload failed before a request was made. It now decodes with
  `createImageBitmap`, which takes the file directly.
- **Invite links did not open the join screen.** The server emitted a path-only
  URL for a hash-routed app, so an invitee landed on the login page with no way to
  sign up — and signup is invite-only. The server now emits the hash form, and the
  app translates the old form for links already sent.
- **Editing one occurrence of a recurring event deleted that occurrence's reminder**
  and left it on all the others: the client sent the reminders to the series rather
  than to the override the server had just created.
- A new build could never reach an open tab: the version check reloaded without
  asking the service worker to update, so the page came back running the same cached
  code — and, having spent its one-reload-per-version guard, stayed on the old build
  indefinitely. This one cost two debugging detours during the work itself.
- The month title was rendered *underneath* the view switcher between roughly 560px
  and 1050px — invisible at 640px and at 900px, which is exactly where the desktop
  sidebar appears.

Everything found but not yet fixed is written down in
[docs/known-issues.md](docs/known-issues.md).

## [0.1.0] — 2026-07-27

First working version: in use by one household, not yet by anyone else.

### Added

- Shared calendars with month, week and agenda views, and per-calendar filtering.
- Recurring events — daily, weekly, monthly and yearly with intervals, nth weekday,
  and last-day-of-month — with *this / this and following / the whole series* editing.
- Per-user reminders over Web Push and email, a configurable daily digest, and activity
  notifications with an optional once-a-day summary.
- A durable notification outbox with at-least-once delivery and a defined catch-up
  policy after an outage.
- Installable PWA: offline reading, dark and light themes, French and English.
- Colouring events by label or by participant; cover pictures for calendars.
- Invite-only signup by shareable link; argon2id passwords; password reset by email.
- French public holidays, computed rather than tabulated.
- `almanack backup`, taking integrity-checked snapshots with generational retention.
- `almanack bootstrap`, creating the first account on an empty database.
- `/healthz`, systemd readiness and watchdog support.

### Known limitations

- No CalDAV, no ICS import or export, no synchronisation with Google or Outlook.
- No per-event chat, photo albums or shared lists.
- No home-screen widgets — a PWA cannot provide them.
- Never upgraded in place: expand/contract migrations are implemented and tested, but
  no release has yet followed another on a live database.

[Unreleased]: https://github.com/d-weber/almanack/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/d-weber/almanack/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/d-weber/almanack/releases/tag/v0.1.0
