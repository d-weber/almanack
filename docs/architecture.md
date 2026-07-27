# Architecture

How Almanack is built, and why it is built that way. If you are here to change something,
read [CONVENTIONS.md](../CONVENTIONS.md) as well — it is the binding version of the rules
this document explains.

## The shape of it

```
[phones and browsers] ──HTTPS──> [your reverse proxy] ──localhost──> [almanack] ──> [almanack.db]
                                                                        │
                                                                        ├──> push services (FCM/Apple/Mozilla)
                                                                        └──> local MTA ──> your mail provider
```

One process, one file of state. The PWA is compiled into the binary with `go:embed`, so
there is nothing to serve separately and nothing to keep in sync with the server.

```
cmd/almanack           the binary: subcommands, server wiring, seed, bootstrap, backup
internal/domain      core types, and the Date type that makes the all-day bug unrepresentable
internal/store       SQLite: schema, migrations, and every SQL query in the app
internal/recur       recurrence expansion — pure functions, no I/O
internal/events      occurrences, scoped edits, series splitting
internal/notify      the planner, the durable outbox, delivery, boot catch-up
internal/webpush     RFC 8030/8291/8292, written from the RFCs
internal/httpapi     HTTP server, middleware, handlers, PWA serving
internal/i18n        fr/en catalogs (shared with the browser) and French date formatting
internal/holidays    French public holidays, computed rather than tabulated
internal/imgproc     avatar and cover-image processing without an image library
internal/auth        argon2id passwords, token minting
internal/clock       the only source of "now"
internal/deps        no code — the dependency policy, expressed as a test
web/                 the PWA, plus the go:embed directive that compiles it in
```

## The dependency rule

Two direct third-party Go modules, and zero in the browser:

- `modernc.org/sqlite` — a pure-Go SQLite driver, so `CGO_ENABLED=0` holds and the binary
  stays a single static file.
- `golang.org/x/crypto` — argon2id only. HKDF comes from the standard library's
  `crypto/hkdf`.

Honest footnote: `modernc.org/sqlite` brings about nine transitive modules of its own, so
the real count of code this project promises to keep compiling is around eleven, not two.
`deps_test.go` pins both the direct allowlist and that transitive closure, and fails the
build if either changes — including if a `package.json` ever appears.

The browser's zero is literal. No framework, no bundler, no CDN, no web fonts, no icon
font. The only build in the project is `go build`.

This is not minimalism for its own sake. Every dependency is a thing that will need
updating during years when nobody is paying attention, and the failure mode of an
unattended calendar is silent.

## Dates, which are the whole problem

Most of the difficulty in a calendar is time, so the rules are structural rather than
advisory.

**All-day events are dates, not instants.** `domain.Date` is a year/month/day triple with
no timezone. Storing an all-day event as midnight produces the classic bug — midnight in
Paris on the 15th is 22:00 UTC on the 14th, so any later UTC conversion moves the event a
day earlier. Timed events are stored as UTC instants; all-day events are stored as `DATE`
columns; a schema `CHECK` makes the wrong combination impossible to write.

**One timezone decides what day it is.** Every "is this today", every digest boundary,
every occurrence identity is computed in the configured family timezone, on the server and
in the browser alike. The frontend passes `timeZone` to every `Intl` call; the device
timezone is never used, so a phone in Lisbon still shows a Paris appointment at its Paris
time.

**Recurrence expands in local wall-clock, then converts.** A weekly 16:30 event stays at
16:30 across a daylight-saving change, which means its UTC instant shifts. The test that
proves it fails loudly if someone "simplifies" the expansion to add seven days to a UTC
timestamp.

**Week numbering for recurrence is always Monday-based.** The per-user "week starts on"
preference is display only and never reaches `internal/recur`; if it did, two members
would see different occurrences of the same event.

**The hour that happens twice is not a name for a moment.** When the clocks go back, a wall
time inside the repeated hour describes two instants, and `wallToInstant()` in the browser
answers the later one. That is the right default for a time somebody has just typed and the
wrong one for a time merely read back off a form, so the event editor keeps the instants it
loaded an event with for as long as the corresponding date and time fields are untouched.
Opening an event in that hour and saving it therefore cannot move it, and an event lying
across the changeover — whose wall clock runs backwards — is still editable. Type into a
field and the wall clock wins again, later pass and all. The rule is asserted end to end in
`e2e/dst-fallback.spec.js`, which is where browser date behaviour gets tested: the frontend
takes no dependencies, so there is no JavaScript unit runner to put it in.

### The recurrence policy table

These were decided deliberately, because every one of them has a plausible opposite:

| Case | Behaviour |
|---|---|
| Monthly on the 29th/30th/31st when the month is shorter | **Skip** that month (RFC 5545 and Google Calendar do this) |
| Yearly on 29 February | **Clamp to 28 February** — the opposite of the rule above, because a birthday must not vanish three years in four |
| `UNTIL` | Inclusive |
| Interval anchoring | The series start is the anchor, for every frequency |
| Monthly modes | Exactly one of: day-of-month, nth weekday (`1..5`, `-1` = last), or last calendar day |

`internal/recur` is pure functions over dates with no I/O, which is why it can be tested
exhaustively. Its test suite includes cases derived from a calendar independently of the
implementation, and the five policies above were mutation-tested to confirm the suite
actually catches their opposites.

### Occurrences are never stored

A recurring event is a template plus a pattern. Occurrences are computed on read within
the window being displayed, so nothing can drift out of sync with the series that produced
it. One database round trip fetches everything a month needs — five statements, never a
query per occurrence.

Editing one occurrence writes an *override*: a standalone event plus a row saying "on this
date, use that instead", or "on this date, nothing". The series is untouched, which is what
makes repeated single-occurrence edits safe.

An override can also move an occurrence outside the span of the series it belongs to —
past the end of term, or back before the first lesson — and the copy holding it is not
readable as an event of its own, because it belongs to its series. So a series carrying an
exception is read for every window regardless of its own dates, and the query that fetches
a month is deliberately coarse in the same direction: it also reads series that ended
shortly before the window, in case a multi-day occurrence reaches into it. Expansion
decides what is actually visible. Too wide costs a few rows; too narrow loses events.

That standalone event is the id the API then publishes for the occurrence, so the *second*
thing anyone does to an edited occurrence arrives addressed to the copy rather than to the
series — and a copy carries no pattern of its own. Every operation therefore resolves an id
back to the (series, date) pair it stands for before acting, in the server rather than in
the client: the identity of an occurrence is a fact about the data, and a client that had
to reconstruct it would be one release away from getting it wrong again. Not doing so is
how "delete this occurrence" deleted the exception instead of the occurrence, and brought
it back at its original time.

"This and following" **splits the series**: the original gets an end date the day before,
a new series starts at the split, overrides at or after the split move across, and every
member's reminders are copied. Missing any one of those steps has a specific, nasty
symptom — a cancelled future occurrence coming back to life, or half a series silently
losing its reminders — and each has a test named after it.

**A scoped edit is one transaction.** Those steps are several writes, and every one of
them runs on the request's context, so a phone that loses its signal part-way through
really does stop the sequence — this is not only a crash window. Each flow therefore runs
inside `Store.InTx`, which hands it a copy of the store whose statements all go through
one transaction; interrupted, the edit leaves nothing behind rather than a series that
was capped without its replacement ever being created. The activity row goes in the same
transaction, because change notifications are planned from the log rather than from the
edit, and a committed edit whose log row was lost is a change nobody is ever told about.
Deciding what to write — validating the new pattern, re-anchoring it, working out which
exceptions still have a date — happens before the transaction opens, since SQLite takes
its write lock at `BEGIN`.

**A range read is one snapshot.** The five statements that draw a month — singles, series
templates, their exceptions, the copies those exceptions point at, everyone's
participation — run inside a read transaction, so the answer is the database at one
instant rather than at five. Against the pool they are independent, and an edit committing
between two of them is observed half-applied: an occurrence drawn twice, once as itself
and once inside its series. Making scoped edits atomic closed every interleaving anyone
could reproduce, and what was left was a transient that healed on the next poll — but
"correct because the writers happen not to land there" is not a property that survives the
next feature. The transaction is read-only, which matters more than it sounds: the driver
turns that into a plain deferred `BEGIN` and takes no write lock, so a month view and a
writer still proceed together. A read that took the write lock would queue every writer
behind every render, and there is a test that writes from a second connection while a read
transaction is open to say so.

## Notifications

The only materialized derivative of an event anywhere in the system is the notification
outbox, and it is reconciled on every planning pass.

**The planner** walks forward 48 hours and writes rows for reminders, digests, activity
notices and summaries. A `UNIQUE` constraint on `(user, kind, source_ref, due_at)` makes
re-planning free rather than duplicating, so idempotency is structural rather than a
property somebody has to remember to preserve.

**A row says what it is for, not what it will say.** Structural idempotency cuts both ways:
a later pass over the same window produces the same key and therefore cannot rewrite a
payload it has already written. So anything that is still changing when the row is written
is left out of it. A reminder names an event that exists now and carries it; a digest and a
summary name only the day they cover, and their contents — the day's agenda, the count of
changes, whether a quiet day is worth sending at all — are read from the calendar at
delivery, along with the wording, per recipient and in their language. Two days is long
enough for a day to be rearranged twice.

**Delivery is at-least-once.** `sent_at` is set only after a push service or the MTA
accepts the message. A crash between sending and marking may duplicate a notification —
that is the correct trade, because a duplicate reminder is annoying and a missed one is the
failure this whole application exists to prevent. The tempting "fix" of marking before
sending silently converts it to at-most-once.

**Acceptance is counted per channel, because the channels fail independently.** A row is
finished when every channel the recipient has took the message, not when any one of them
did. Counting a single acceptance across both let the untrustworthy channel vouch for the
other: push answers 201 for a subscription iOS revoked weeks ago, so a push "success"
alongside an MTA refusal is a notification nobody ever receives. `email_sent_at` records
the mail leg on its own, which is also what makes retrying safe — the next attempt sends
what is still owed and leaves the mail alone, instead of mailing the family the same
reminder every time it tries.

**A row is retired by time, never by an attempt count.** The question a failing row asks is
whether the thing it announces still matters, and the staleness policy above already
answers it for every kind: a reminder while its event is ahead, a digest or summary for
four hours, an activity notice for twenty-four. A counter answers a different question, and
answered it badly — ten attempts one tick apart is five minutes, so an afternoon's trouble
at a push service permanently retired a reminder for the next morning. Retries instead back
off, doubling from thirty seconds to an hour, because retrying for as long as a row stays
relevant is otherwise thousands of requests per subscription and push services rate-limit.
The consequence worth knowing: a row whose push leg never succeeds ends up recorded as
`skipped` even though its email went out hours earlier. `email_sent_at` is where that truth
is kept — one row does not get a third state to say so.

**Boot catch-up** is a defined policy, not best effort. After an outage the server
backfills the window the planner never materialized (otherwise reminders in that gap simply
never existed), delivers overdue reminders whose events are still in the future, marks the
rest skipped with a reason rather than dropping them silently, and discards digests more
than a few hours stale. There is a test that switches the server off for a week and asserts
each of those outcomes.

**Email is a parallel channel, not a fallback.** iOS push subscriptions die silently, with
the push service still returning success, so a design that waits for a delivery error would
wait forever. Email reminders default to on, and the client re-confirms its push
subscription on every app open so a dead one is detected from the client side.

**Push is implemented here**, from RFC 8291 (encryption), RFC 8292 (VAPID) and RFC 8030
(delivery). It is about 700 lines and it passes the RFC's own published test vectors byte
for byte. Two traps worth knowing if you touch it: the VAPID signature must be the raw
64-byte R‖S concatenation and not ASN.1, and the info strings carry a trailing NUL byte.
Push *services* drift over the years even though the RFCs do not, so this is the one part
of the system that should be expected to need an occasional patch.

## Storage

SQLite in WAL mode, with instants as RFC 3339 UTC text and dates as `YYYY-MM-DD` text, so
that a database opened in fifteen years explains itself without this document.

`internal/store` is the only package containing SQL, and the only one that holds a
`*sql.Tx`: a caller that needs several writes to land together asks for `InTx` and gets a
transaction-scoped copy of the store, rather than a bespoke store method per sequence.
Nesting one inside another joins the transaction already open, because a four-connection
pool holding an exclusive write lock has no second connection to give.

Migrations are numbered, embedded and immutable, applied in a transaction at startup. They follow expand/contract — each release
stays readable by the previous binary for one version — which is what makes a rollback
"put the old binary back" rather than "restore a backup". Concretely: 0003 adds a nullable
column to `notification_queue`, and every statement the 0.2.0 binary issues names the
columns it wants, so that binary runs against the upgraded file with nothing to undo. What
it will not do is start there by itself: the binary refuses to open a schema newer than it
knows, so a mistaken downgrade fails loudly instead of corrupting data, and a deliberate
one is a decision somebody takes rather than a surprise. Every migration is proved against
a database a shipped release really wrote, checked in under `internal/store/testdata/`.

Backups are `almanack backup`: `VACUUM INTO` a temporary file, run `PRAGMA integrity_check`
**on the output**, fsync, then rename atomically. Checking the copy rather than the source
is the point — a backup that faithfully preserves a corrupt page is not a backup — and a
non-zero exit is the signal your monitoring should act on.

## The browser side

Hand-written ES2020 modules. Views render through an `h()` helper that inserts text as text
nodes and attaches handlers with `addEventListener`; the CSP ships without `unsafe-inline`,
so an inline `onclick` is a dead button rather than a shortcut, and `innerHTML` with server
data is forbidden outright.

The service worker precaches the shell keyed by a build hash the server computes from the
embedded assets, and serves `/api/` network-first with a cache fallback so the last-seen
calendar is readable offline. Its push handler always displays a notification, including a
generic fallback when a payload cannot be parsed — iOS revokes a subscription after roughly
three pushes that display nothing, so a silent push is a bug and never an optimisation.

Those cached `/api/` responses belong to a session rather than to the device. Ending one —
the sign-out button, a 401 from the server, a boot that could not load a session — goes
through `clearSession()` in `js/state.js`, which asks the worker to delete every `/api/`
entry it holds; the shell is untouched, since it is the same for everyone and the login
screen is served from it. The entries are also capped at the most recent sixty, evicted
oldest first, so an install that lives on a home screen for a year does not accumulate
every range and search it was ever asked for.

If you add a JavaScript module, add it to the precache list in `web/sw.js`. That list is
explicit by design, and forgetting it breaks offline use quietly.

## Security posture

Invite-only signup, so there is no open registration on a WAN-facing service. Sessions,
invites and password-reset tokens are 256-bit random values stored only as SHA-256 hashes;
passwords use argon2id with RFC 9106 parameters. CSRF defence is `SameSite=Lax` plus a
required `X-Requested-With` header enforced centrally, which works because mutations are
never `GET` — a route-table test asserts that, with one documented exception for a
development-only login link that is never registered outside dev mode.

Uploaded images are size-capped, sniffed, dimension-checked *before* decoding (a 200-byte
PNG can claim 20000×20000), re-encoded server-side, and served with `nosniff`.

Permissions are deliberately flat: every member of a calendar can edit anything in it, and
only the creator can remove members. This is a tool for people who already trust each
other, and the alternative is a permission model nobody wants to administer for their own
household.

## What it deliberately does not do

Not a roadmap — these were decided against, and the reasoning is why the rest is simple:

- **No CalDAV, no ICS import or subscription, no Google/Outlook sync.** This is the biggest
  omission and the one most likely to matter to you. It also means no interoperability
  surface to maintain for a decade.
- **No per-event chat, photo albums, or shared lists.** TimeTree's social layer is most of
  TimeTree; leaving it out is most of why this is small.
- **No native apps and therefore no home-screen widgets** — a PWA cannot provide them on
  either platform.
- **No multi-tenancy, no scaling story.** One family, one server, tens of users.

If you want any of these, forking is reasonable and the licence encourages it.

## Known risks

| Risk | What absorbs it |
|---|---|
| Push services drift over the years | The sender is in-tree and small; `/healthz` counts failing subscriptions and the daily heartbeat mail names the service; a browser issuing endpoints on a new host is refused by name in the log (`ALMANACK_PUSH_HOSTS`); email runs in parallel |
| Mail providers keep retiring authentication methods | The binary only ever speaks SMTP to `127.0.0.1`; provider churn is an OS config edit |
| A pure-Go SQLite driver has a rare correctness bug | Integrity-checked backups with generational retention; `ncruces/go-sqlite3` is the CGO-free escape hatch |
| Apple changes PWA or push policy | Email is a first-class channel; worst case this is a website that emails you |
| The clock is wrong after a long outage | The scheduler refuses to run before its own build date; the unit should order after time sync |
| Nobody maintains this for years | One binary, one file, no dependencies to chase, and tests that fail loudly rather than subtly |
