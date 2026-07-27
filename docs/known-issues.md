# Known issues

Findings from an adversarial review of the built system (six independent passes:
calendar correctness, security, notification reliability, the data layer, the browser,
and operational readiness). Everything here was **reproduced** — each item had a failing
test or a request transcript behind it, not a suspicion.

The ones already fixed are in the changelog. These are the ones still open, worst first.
If you are looking for somewhere to help, this is the list.

## High

**Scoped edits are not atomic.** Splitting a series, overriding an occurrence,
cancelling one, and deleting a series each perform several writes through separate
transactions. An interrupted request — a dropped mobile connection is enough — can leave
a series with no replacement half, a duplicate standalone event, or a "cancelled"
occurrence that reappears. The ordering bug that made this reachable *without* an
interruption is fixed; the underlying non-atomicity is not. The fix is store-level
methods (`SplitSeries`, `SetOccurrenceOverride`, `CancelOccurrence`, `DeleteSeries`) that
do their work inside one transaction, in the same shape `CreateEvent` already uses.

**Deleting a calendar leaves orphans that still fire.** The foreign-key graph runs
`events.recurrence_id → recurrences`, so cascading events away never removes the
recurrence rows; reminders scoped to those recurrences survive with them, and
`notification_queue` is keyed only on the user. The result is queued notifications for a
calendar that no longer exists, and rows the planner walks on every pass forever. The
proper fix is a `calendar_id` on `recurrences` with `ON DELETE CASCADE`, plus clearing
the calendar's queued rows in the same transaction.

**The activity feed loses entries.** Pagination uses `at < before` with instants stored
at second precision, so any row sharing the boundary second with the last row of a page
is skipped permanently — and the notification planner advances its cursor past rows it
never saw, so those changes are never announced. The cursor needs to be `(at, id)`.

**Occurrences moved outside their series' envelope disappear.** `EventsInRange` filters
series on `dtstart <= to AND (until IS NULL OR until >= from)`, three lines below a
comment explaining that overrides can move an occurrence *outside* that envelope. Move
the last lesson of a series to the following month and it is invisible from both
queries. Widen the predicate to include series with any override, and to account for
multi-day event spans.

**Editing an already-edited occurrence addresses the wrong event.** The API identifies
an overridden occurrence by the override copy's own id, so a second edit prunes the
wrong queue rows (the old reminder still fires at the old time), and deleting it removes
the override row through the schema cascade — which *restores* the occurrence instead of
cancelling it. `internal/events` needs to resolve an override copy back to its series
before acting.

## Medium

**Digest contents are frozen when the row is materialized**, up to 48 hours early. An
event added on Monday afternoon is missing from Tuesday's 07:30 digest. Summaries already
resolve their content at delivery for exactly this reason; digests should do the same.

**Retirement of failing notifications is attempt-based, not time-based.** Ten failures at
a 30-second tick means a six-minute push outage permanently retires a reminder for an
event still hours away. It should keep retrying while the event is ahead.

**A push success masks an email failure.** The row is marked sent as soon as any channel
is accepted, so a transient MTA problem loses the email — the channel that exists
precisely because push dies silently. Tracking the two channels separately needs one
nullable column.

**Other notification gaps:** a failed subscription lookup marks a row *sent* rather than
retrying; the activity cursor advances past a row whose fan-out failed, losing that
notification; the daily summary ignores `participating_only`, which the per-change path
honours.

**`PUT /events/{id}` silently ignores adding or removing a repeat.** "Repeat weekly" on an
existing event returns 200 and stores nothing. Either implement both transitions or
reject the request.

**Range reads are not snapshots.** Five queries on the pool with no enclosing read
transaction can observe an occurrence edit half-applied, showing an event twice or not at
all. Needs a read-transaction variant that does not take the write lock.

**Removing a member leaves them attached** as a participant of the calendar's events, and
their reminders reactivate if they are ever re-invited.

**Authenticated blind SSRF through push subscriptions.** A member can register a
subscription whose endpoint points at an internal address and have the server connect to
it. Web Push is inherently "POST to a client-supplied URL", so the fix is to refuse
private, loopback and link-local addresses after DNS resolution, and to refuse redirects.

**The documented rollback story does not work.** The migrations genuinely are expand-only,
but the binary refuses to start against any schema newer than it knows, so putting the old
binary back fails anyway. Either record a minimum-binary version per migration, or say
plainly that a downgrade needs a restore.

## Browser

**The cached calendar outlives the session.** The service worker caches every
successful `GET /api/`, including `/api/v1/me`. With the cookie gone and the server
unreachable, the app boots authenticated and renders the whole family's calendar from
CacheStorage. The API cache is also never pruned — only wiped on a version change — so
a home-screen install accumulates every range and search query indefinitely. Skip the
cache for `/api/v1/me` and `/api/v1/auth/`, and purge `/api/` entries on logout.

**An event spanning the daylight-saving fall-back hour cannot be saved.** Its wall-clock
end precedes its wall-clock start, so the editor's validation refuses it even when
nothing was changed — the event becomes permanently uneditable. Validate against the
original instants when the time fields are untouched, or carry the duration through the
editor rather than round-tripping both endpoints.

**Multi-day bars are unlabelled after the first week**, including to a screen reader —
they are buttons with no accessible name. Repeat the title on continuation segments, or
at least set `aria-label`.

**The agenda view leaks an IntersectionObserver per visit**, and with it the whole
detached view it closed over, because it only disconnects when paging completes. Views
need a teardown hook called from `show()`.

**Accessibility gaps in the overlays and editor:** no focus trap, no focus restore on
close, and no accessible name on the dialog; the timed editor's four date and time
inputs have no programmatic label; event chips are 20px tall, under the WCAG target
size; and at 320px the app bar wraps to three lines with the two month arrows on
different rows.

**`safeHref` can be bypassed** by control characters inside the scheme
(`java\tscript:`), because HTML URL parsing strips them after the check runs. Nothing
executes today — the CSP has no `unsafe-inline` and no reachable sink takes user data —
but the helper does not do what its contract says. Strip `[\u0000-\u0020]` before the
scheme test.

## Low

- A recurring event that spans a daylight-saving change keeps its start's wall clock but
  not its end's, so one occurrence a year is an hour longer or shorter. Defensible, but
  the docs assert the opposite.
- Two concurrent `agenda backup` runs: the second deletes the first's in-progress file.
- Accent folding covers `æ`/`œ` but not `ø`/`ß`.
- `SQLITE_BUSY` is not mapped to a retryable status; it surfaces as a 500.
- The watchdog is fed once per scheduler tick, so `WatchdogSec` below `2 × AGENDA_TICK`
  causes a restart loop. Needs documenting and a startup warning.
- `READY=1` is sent before the listener is bound, so a port clash marks the unit active
  and then exits.
- `/healthz` reports `ok` when the data directory has vanished (statfs failure is treated
  as fine, and the database check does not touch storage).
- Backup snapshots are created world-readable under the process umask.
- The clock plausibility floor is a hardcoded 2026 constant rather than the build date, so
  it decays every year.
