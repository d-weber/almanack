# Changelog

Notable changes to this project. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
- `agenda backup`, taking integrity-checked snapshots with generational retention.
- `agenda bootstrap`, creating the first account on an empty database.
- `/healthz`, systemd readiness and watchdog support.

### Known limitations

- No CalDAV, no ICS import or export, no synchronisation with Google or Outlook.
- No per-event chat, photo albums or shared lists.
- No home-screen widgets — a PWA cannot provide them.
- Never upgraded in place: expand/contract migrations are implemented and tested, but
  no release has yet followed another on a live database.

[Unreleased]: https://keepachangelog.com/
