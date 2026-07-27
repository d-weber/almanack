# Changelog

Notable changes to this project. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **[docs/install.md](docs/install.md)** — a worked install, from downloading a binary to a
  checklist that tells you it is working. A five-minute local trial that needs no domain and
  no TLS, then a server install with systemd units, Caddy and nginx configuration, mail
  relaying, backup timers and failure alerting, plus a section for people with no public
  domain name. Every command in it was run; the unit files pass `systemd-analyze verify`.
- **[docs/RESTORE.md](docs/RESTORE.md)** — the restore runbook `docs/development.md` had been
  pointing at for some time without it existing.

- **Tests for the four packages that had none**, and for the two subcommands that had
  almost none. `internal/auth` (0% → 94%) pins the argon2id parameters as literals, so a
  silent weakening of the memory or time cost fails the build, and asserts tokens are
  stored as SHA-256 hashes and never in the clear. `internal/config` (0% → 99%) covers the
  strictness 0.2.0 introduced, and cross-checks the parser's keys against
  `almanack.conf.example` in both directions so the example cannot drift from the code.
  `internal/mailer` (0% → 100%) covers the header-injection case 0.2.0 fixed, at both the
  validation and the encoding layer. `cmd/almanack` (3% → 38%, with `backup.go` at 81%)
  proves a snapshot is a real database with its rows still in it, and that `--prune` with
  zero retention deletes nothing — it once deleted everything, including the snapshot it
  had just reported as a success.
- **Upgrading an existing calendar is now proved by a test rather than promised by a
  document.** The suite had two migration tests, and neither covered the thing that actually
  happens to a household: run 0.2.0 for a year, drop a newer binary on the same file, and find
  out. One reopened a database this binary had created seconds earlier — head opening head —
  and the other checked that a database from the *future* is refused. So a database the v0.2.0
  binary really wrote is now checked into the repository as SQL text
  ([`internal/store/testdata/v0.2.0.sql`](internal/store/testdata/v0.2.0.sql)): four people
  with real argon2id hashes, three calendars with their members, renamed labels and a cover
  image, timed and all-day events, a weekly series with one occurrence moved and another
  cancelled, reminders of both shapes, a signed-in session, a live invite and a revoked one, an
  outbox caught mid-flight, an activity log, holiday corrections. `make check` replays it into
  an empty file, opens it with the current binary, and then insists: every embedded migration
  applied, `integrity_check` and `foreign_key_check` clean, every table holding exactly the rows
  it held before, and the family readable back through the store API — Leo's password still
  verifies, the seaside holiday is still seven inclusive days and not a timezone, 4 August is
  still moved to the evening and 18 August is still cancelled. It is text rather than a `.db`
  file so it can be read in a diff, and in ten years, by someone with only a text editor.
  It is also self-maintaining: the version it was captured at is pinned in the test, so when
  0003 lands this starts exercising a genuine 0002 → 0003 upgrade with nothing to edit, and
  anyone who "fixes" a failure by regenerating the fixture at head gets told what they have
  just switched off.
- **Coverage floors, enforced.** `make check` now ends with `make cover-check`, which fails
  if a package drops below the floor recorded for it in
  `.github/scripts/check_coverage.py`, or if a package appears with no entry at all.
  Coverage that is only reported rots quietly; adding uncovered code now means writing the
  test or lowering the floor in the same commit, which is a visible decision in a diff.
- **The browser tests run in CI.** The Playwright suite existed but ran only if someone
  remembered to, which for the three 0.2.0 bugs that were reachable only through a real
  browser is the same as not existing. CI now seeds a demo family, starts a server and
  runs it.

### Changed

- **Planning moved to the issue tracker.** What was `docs/known-issues.md` is now one
  GitHub issue per defect, and the road to 1.0 is milestones 0.3 to 1.0 — the data layer,
  notifications, the browser, then operations, followed by a soak through both
  daylight-saving changes. [#28](https://github.com/d-weber/almanack/issues/28) is the
  1.0 release criteria. Each item was re-reviewed against the code on the way across:
  several proposed fixes were cut down to smaller ones, two were dropped as not worth the
  code, and one had already been fixed.

### Removed

- `docs/known-issues.md` and the roadmap that briefly replaced part of it. A file listing
  what is broken goes stale between releases; an issue closes when the fix lands.

### Fixed

- **Deleting an occurrence you had already edited brought it back.** Editing a single
  occurrence stores a separate copy of the event, and from then on the app addressed that
  occurrence by the copy's own id. Deleting the copy took the exception with it, so the
  occurrence reappeared at the series' original time and without the edit — "delete this
  one" was how you made it come back. The same mix-up hid the series from an occurrence
  that had already been edited: opening it no longer asked *this / this and following /
  the whole series*, so nothing done to it afterwards could reach the rest of the series,
  and the reminder stayed queued for the time the occurrence used to be at. Two clicks
  from the month view, silent, and repeatable. An edited occurrence is now resolved back
  to the date in the series it stands for before anything is changed.
  ([#1](https://github.com/d-weber/almanack/issues/1))
- **An occurrence you moved out of its series' own dates vanished.** Drag the last lesson
  of term into the following month and it was simply gone: not on the new date, not on the
  old one, not anywhere. A series was read back only for the window between its first
  occurrence and its last, and an edited occurrence lives outside that window as soon as
  you move it past either end — the copy holding it is deliberately hidden as an event of
  its own, because it belongs to its series, and the series was no longer being asked.
  Both ends were affected, so bringing the first occurrence of next term forward into this
  month lost it the same way, and no reminder was ever queued for it either — the planner
  reads the calendar through the same window. A series with an exception is now read
  whatever its own dates say. The related case with no edit involved is fixed too: a recurring three-day
  trip that started on the series' last day and ran into the next month disappeared from
  that month, where the same trip as a one-off event would have been shown.
  ([#2](https://github.com/d-weber/almanack/issues/2))
- **Losing your connection half-way through "this and following" could take half the series
  with it.** Splitting a series is several writes — end the original the day before the
  split, start the replacement at it, move any edited or cancelled occurrences across, copy
  everyone's reminders — and each one travelled on the request's own connection. A phone
  that went into a lift between the first and the second left the original series ended and
  its replacement never created: every swimming lesson from the date you were editing
  onwards silently gone, with an error on screen that said nothing about it. Editing an
  occurrence, cancelling one and deleting a series had smaller versions of the same problem,
  leaving a duplicate or an orphaned copy behind rather than losing anything. Each of those
  edits is now a single transaction, so an interrupted one leaves the calendar exactly as it
  was. The activity entry is part of it too: an edit can no longer be saved while the
  notification that tells everyone about it is lost.
  ([#3](https://github.com/d-weber/almanack/issues/3))
- **Deleting a calendar, and removing somebody from one, left rows behind.** Deleting a
  calendar took its events with it but not the repeat patterns behind them, nor the
  reminders hanging off those, nor — the half anyone would notice — the notifications
  already prepared for the next two days: a reminder for a swimming lesson could still
  arrive from a calendar that had been deleted the day before. Removing a member, or
  leaving a calendar, deleted the membership and nothing else, so an ex-member stayed
  listed on other people's events, where the app itself refuses to put someone who is not
  a member, and their reminders sat dormant until somebody invited them back, at which
  point they started firing again. Both are now a single transaction that clears out what
  the calendar or the membership was holding up, and both stay scoped: the same person's
  events and reminders in the calendars they are still in are untouched, and notifications
  that have already gone out are left alone, because the outbox is also the record of what
  was sent. Neither needed a change to the database.
  ([#4](https://github.com/d-weber/almanack/issues/4))
- **Ticking "repeat weekly" on an event that already existed did nothing at all.** The
  editor sent the change, the server answered 200, the screen returned to the calendar —
  and the event was still a one-off. Unticking the repeat on a series was the same in
  reverse: the series carried on exactly as before, and nothing said otherwise. Both are
  now refused with a message that says so, and the editor no longer offers the choice for
  an event that exists; the pattern of an existing series can still be changed as it always
  could. This replaces a silent no-op with an honest refusal — it does not add the feature.
  Doing that properly means moving the reminders people have already set onto the new
  series, or every occurrence gets notified twice, and answering what becomes of the
  occasions somebody had edited by hand when the repeat goes away, since the rows holding
  those exceptions go with it. Both belong in a change of their own. For now, turning a
  one-off into a series means creating it again.
  ([#5](https://github.com/d-weber/almanack/issues/5))
- **A month view could draw the same event twice.** Drawing a month takes five queries —
  the one-off events, the repeating series, the occurrences somebody has edited or
  cancelled, the copies carrying those edits, everyone's participation — and each one went
  to the database on its own. An edit committing between two of them was therefore seen
  half-applied: the copy holding a moved occurrence could pass the first query as an
  ordinary event and then arrive a second time inside its series, so the same swimming
  lesson showed up twice, at two different times, on the same day. It is worth being plain
  about how narrow this had become. Making each scoped edit a single transaction (above)
  closed every interleaving that could actually be reproduced; what remained needed
  somebody else in the household to save an edit inside the few milliseconds one render
  spends between two of its queries, and it healed on the next poll with nothing stored
  wrong. The five queries now run inside one read transaction, so what comes back is the
  calendar at a single instant rather than at five — self-consistent by construction rather
  than because writers happen not to land there. That transaction is read-only and takes no
  write lock, so a month view still holds nobody up; there is a test that writes from a
  second connection while one is open, because a read that queued every writer behind every
  render would be a considerably worse bug than the one being fixed here.
  ([#6](https://github.com/d-weber/almanack/issues/6))
- **An entry could go missing from the activity feed, and a change could go out to nobody.**
  Two changes made in the same second — saving an event and correcting it straight away, or
  two people typing at once — carry the same timestamp, because the log records the second
  and not the millisecond. The feed asked for the next page by the time of the last entry
  it had, so anything sharing that second was on the wrong side of "older than this" and was
  never shown again: not on the next scroll, not on a reload, not ever. The screen looked
  complete, which is the part that matters — there was nothing to suggest an entry was
  missing. The notification planner read the same feed to decide who to tell about what, and
  after a long enough outage it stepped over changes the same way, so the answer for those
  was nobody. Both now page by the entry's id, which is unique and is the order the changes
  were actually made in, so a page boundary can fall anywhere without dropping a row. The
  planner's copy of the paging is gone entirely: it reads forwards from where it got to, one
  query, and picks up in the same place next time rather than abandoning the middle of a
  backlog it could not finish. The activity endpoint takes `before_id` in place of `before`;
  `before` still works, and still cannot page through a shared second, which is why it is no
  longer what the app sends. Nothing changed in the database.
  ([#7](https://github.com/d-weber/almanack/issues/7))
- `tools/timetree-export` referred to a `docs/timetree-migration.md` that has never existed
  under that name.

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

Everything found but not yet fixed is in
[the issue tracker](https://github.com/d-weber/almanack/issues) (it was
`docs/known-issues.md` at the time of this release).

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
