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
- **A push subscription may now only point at a push service.** The endpoint a browser hands
  over is checked against a list of the four services that issue them — Google's, Mozilla's,
  Apple's and Microsoft's — and anything else is refused when the device registers, with a
  400 the app reports and a warning in the log naming the host. Default-deny rather than a
  list of addresses to avoid: the set of push services is short and known, where the set of
  things not to dial is neither, and a denylist that is wrong is wrong silently. The cost is
  real, and falls on the day a browser starts issuing endpoints somewhere new, so it is paid
  for openly: `ALMANACK_PUSH_HOSTS` takes a comma-separated list of hosts (`*.example.org`
  matches subdomains) and replaces the built-in one, and `*` accepts anything, which is what
  running your own push service needs. Every subscription in an existing calendar points at
  one of the four already, so upgrading changes nothing for anyone; a subscription whose host
  is not allowed is skipped at delivery rather than deleted, and can still be confirmed and
  removed by the browser that owns it.
  ([#12](https://github.com/d-weber/almanack/issues/12))

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
- **A change to the calendar could go out to nobody.** The planner works through the change
  log in order, telling the people each entry concerns, and then moves a marker past
  everything it has done. When the database refused one of those writes for a moment — a
  lock held a fraction too long is all it takes — the planner noted the error, carried on to
  the next entry, and moved the marker past both. Nothing is ever read from behind that
  marker, so the entry that failed was not retried on the following tick, or on any tick
  after it: the appointment was on the calendar, the feed showed it had been added, and not
  one person was notified. Nothing looked wrong afterwards, which is the part that matters —
  there was no failed notification to find, because it had never been prepared. The planner
  now stops at the first entry it cannot get out and leaves the marker in front of it, so
  that entry and the ones behind it go thirty seconds later on the next tick. Nothing
  changed in the database.
  ([#10](https://github.com/d-weber/almanack/issues/10))
- **The morning digest could describe a day that had since changed.** Tomorrow's digest is
  prepared up to two days before it is sent, and it was prepared with that day's agenda
  already written into it. From that moment the message was fixed: an appointment added to
  the day, moved, renamed or cancelled afterwards made no difference to what arrived at
  07:30. Nothing could correct it either — the outbox is keyed on who is being told, what
  about, and when, so a later pass recognises the message it has already prepared and leaves
  it exactly as it stands. It announced the dentist you had cancelled the evening before,
  and said nothing about the swimming lesson you had added in its place. One case healed
  itself, which is worth naming because it is the one anybody would have tried first: a day
  with nothing on it at all produced no digest, so filling it in afterwards worked as
  expected — unless "even on days with no events" was on, in which case the empty message
  was already waiting. Everything else was frozen, on any day edited within forty-eight
  hours, which is most days. The digest now reads the day at the moment it goes out, exactly
  as the daily summary of changes already did, and "even on days with no events" is answered
  then too, since whether a day is quiet is not known until you look. The planner is
  markedly cheaper for it: it had been expanding everyone's next two days on every
  thirty-second tick and throwing the result away, and a household of six with the digest on
  now plans a pass in about an eighth of the time. Nothing changed in the database.
  ([#8](https://github.com/d-weber/almanack/issues/8))
- **A push that was accepted covered for an email that was not.** A reminder goes out over
  two channels at once, and the outbox recorded only that *something* had taken it. So when
  the mail server was briefly unavailable — a restart, a full disk, a relay refusing
  connections for ten minutes — the push service's cheerful acceptance retired the reminder
  and the email was dropped with a line in the log. That is the worst pairing there is,
  because the email exists precisely to cover for push: a phone that has quietly revoked its
  subscription still has its push service answer "delivered" to the sender, so "push
  accepted, email failed" is a reminder nobody ever receives, filed as sent. Two smaller
  faults sat inside the same decision. If the database read that lists somebody's devices
  failed, the failure was logged, the list came back empty, and the reminder was marked sent
  without one device having been asked — a missed notification that leaves no trace
  anywhere. And a reminder was given up on after ten attempts, which at one attempt per tick
  is five minutes: a push service having a bad afternoon permanently retired the reminder for
  tomorrow morning's dentist, hours before anyone needed telling, and the row was filed as
  undeliverable while both the appointment and every chance of announcing it were still
  ahead. Each channel is now accounted for on its own. A reminder is finished when every
  channel that person actually has took it; a failed lookup leaves it queued for the next
  tick; and it keeps being retried for as long as the thing it announces still matters —
  until the appointment, for four hours for a digest, for a day for a change notice — with
  the wait between attempts doubling from thirty seconds to an hour, so a long outage does
  not become thousands of requests at a push service. Crucially, the leg that already went is
  not sent again: an evening of push failures does not turn into an evening of duplicate
  emails. This is the first change to require one to the database — an existing calendar
  gains a column recording when its email went out, and upgrades in place with nothing to
  do. One thing worth knowing: if push never recovers before the appointment, the reminder
  ends up filed as skipped even though the email arrived. The record of the email is kept
  beside it, rather than inventing a third answer to "did this go out?".
  ([#9](https://github.com/d-weber/almanack/issues/9))
- **A push subscription could point this server at its own network, by way of a redirect.**
  Registering a device hands the server a URL and asks it to post there, which is simply how
  Web Push works; the endpoint was checked for being an `https` URL and nothing else. On its
  own that was less than it sounds — a certificate has to validate, so a plaintext service on
  the internal network cannot complete the handshake and the endpoint learns only whether
  something answered. The redirect was the hole. Go's HTTP client follows up to ten of them,
  does not refuse a downgrade from `https` to plain `http`, and replays the POST body on a
  307 or 308 — so an endpoint that answered "moved" could have the next request delivered to
  the database on the same machine, the router, or a cloud metadata service, and a 301, 302
  or 303 turned it into a GET of any address at all. Any invited member could reach it: their
  own device registration, then the "send a test notification" button. The sender now refuses
  redirects outright and reports the 3xx as the error it is. Real push services answer at the
  endpoint they issued. Separately, `/healthz` needs no session and was reporting delivery
  failures broken down by push service host — a value the person registering a device
  chooses — which answered "did the request I aimed at that address succeed?" to anyone who
  asked. It now reports how many subscriptions are failing and no longer names them; which
  service is failing is in the daily heartbeat mail, which has a recipient rather than a URL.
  ([#12](https://github.com/d-weber/almanack/issues/12))
- **Signing out left the family's calendar on the phone.** The app keeps a copy of the last
  calendar it was shown, so that it still reads on a train with no signal. What it never did
  was give that copy back. Sign out, hand the phone to somebody else, and everything the
  browser had already seen was still on it — the appointments, who was going to them, the
  searches — waiting for the next moment there was no connection. Starting a signed-out phone
  up with no signal was worse than reading it: the saved answer to "who is signed in?" is a
  yes, so the app took it and opened the whole calendar as though nobody had ever left. That
  copy is now deleted the moment a session ends, however it ends — the sign-out button, a
  session the server no longer recognises, or an app that starts and finds it has none. What
  stays is the app itself, which is the same for everybody and is what draws the sign-in
  screen. A device that is still signed in still reads its calendar offline exactly as
  before, which is the point of keeping a copy at all. Separately, that copy no longer grows
  without end: an install that had sat on a home screen for a year was holding every month
  anyone had scrolled to and every search anyone had typed, and now keeps the sixty most
  recent and forgets the rest.
  ([#13](https://github.com/d-weber/almanack/issues/13))
- **One hour a year, an appointment nobody could edit — and, on the same hour, an event that
  moved itself when you corrected its spelling.** On the night the clocks go back, 02:00 to
  02:59 runs twice in Paris, so "02:10" that morning is the name of two different moments an
  hour apart. The editor read an event's times off the clock face and asked which moment they
  were, and the answer was always the second one. An event that ran from 02:50 to 02:10 across
  the changeover — forty ordinary minutes, one on each side of it — therefore came back as one
  that ended before it began, and the editor refused to save it. Not the change you had just
  made: everything, including opening it and pressing Save having touched nothing at all. That
  appointment could never be edited again. The other half was quieter and worse. An event lying
  wholly on the first pass of the hour, 02:10 to 02:50 before the clocks went back, saved
  without a murmur and came out an hour later than it went in. The screen gives nothing away,
  because 02:10 is what both moments are called and the form reads exactly as it did — what
  moves is when the appointment actually happens, so the reminder arrives an hour late and a
  member reading the calendar from another country sees it in the wrong place. Correcting a
  typo was enough to do it. The editor now keeps the moments it loaded an event with for as
  long as the date and time boxes still read what they read when it opened; type into one and
  it resolves off the clock face as before, to the later of the two passes. Carrying the
  event's duration through the editor instead would have changed what the editor means —
  moving the start would drag the end along with it — and still would not have given back the
  moment the event began at.
  ([#15](https://github.com/d-weber/almanack/issues/15))
- **A week-long holiday was announced as, simply, "button".** A multi-day event is drawn as
  one bar per week of the grid it crosses, and only the first of them was given the title —
  the rest were buttons wrapped around an empty label. So the second week of a seaside
  holiday read out as "button" to anyone using a screen reader, and said nothing at all on
  hover, on a bar 20 pixels tall that is too narrow to show its own title anyway. Trips and
  holidays are exactly what somebody scrolls back through a calendar to find. Every segment
  of a bar now answers to the name of its event and carries it as a tooltip, the way the
  holiday bars beside them already did; the title is still written once, in the week the
  event begins, because that is what there is room for. The editor had the quieter version
  of the same fault: the date and time boxes of a **timed** event sit in pairs under a single
  word, "Starts" or "Ends", which belongs to neither of them and cannot say which is which,
  so all four were announced as nothing but their type — two of them "date" and two of them
  "time", in one form. Each now gives its own name in both languages ("Starts, date",
  "Début, heure"). All-day events were never affected, in either place.
  ([#16](https://github.com/d-weber/almanack/issues/16),
  [#18](https://github.com/d-weber/almanack/issues/18))
- **Tab walked straight out of every dialog, and answering one left you at the top of the
  page.** The day sheet, the yes/no confirmations and the *this / this and following / the
  whole series* question are one function between them, and it announced each of them as a
  modal dialog while behaving like nothing of the sort. Four presses of Tab inside the scope
  question — the one the app insists on before it will change anything that repeats — and the
  keyboard was out of the panel and off into the month grid behind it, on controls the
  backdrop covers and nobody can see, with no way back except Shift+Tab counted blind. The
  other half was the way out: the panel closed and the keyboard went to the top of the
  document with it, so answering "just this one" about a swimming lesson meant tabbing the
  whole way back down the page to reach anything at all — and the same after every
  confirmation, and after every day sheet. Then the name, or the absence of one:
  `role="dialog"` was set with nothing to label it, so all three announced themselves as
  "dialog" and no more. The question itself — the words that make *this*, *this and
  following* and *the whole series* mean anything — was left to be found by browsing the page
  the dialog was blocking. All three faults are in one function, `openOverlay()`, so all three
  overlays are fixed at once: Tab cycles within the panel and Shift+Tab cycles back through
  it, whatever opened the overlay gets the keyboard back when it closes — by Escape, by
  tapping the backdrop, or by a button in the panel — and the dialog takes its name from the
  heading it already draws, so the two cannot drift apart. An overlay with no heading falls
  back to a plain "dialog" from the catalogue, and one with nothing to focus at all keeps the
  keyboard on itself rather than letting it out into a page the reader cannot see.
  ([#17](https://github.com/d-weber/almanack/issues/17))
- **The agenda and the activity feed left their paging observer behind when you walked
  away from them.** Both screens load their next page when a sentinel at the foot of the
  list scrolls into view, and both disconnected the `IntersectionObserver` watching it only
  once the list was complete — so leaving before that, which is what one does with a list
  that goes on for a year, left the observer connected to a sentinel that had just been
  dropped from the page. This is untidiness rather than a leak that grows: the browser
  holds an observer's targets weakly, so the pair becomes an island the collector takes,
  and a detached sentinel cannot come into view, so the observer would never have fired
  again either. It is fixed because it is the kind of thing that stops being harmless the
  moment a screen holds something the collector cannot reason its way out of — an interval,
  a listener on `window` — and there was no way for a view to tidy up after itself at all.
  There is one now: a view hangs a `cleanup` function off the node it returns, and the
  shell calls it before mounting the next screen. A view cannot do this alone, because
  nothing tells it that it has been replaced — an observer on a detached target never fires
  again, so there is no moment at which it could notice.
  ([#19](https://github.com/d-weber/almanack/issues/19))
- **Security: a link on an event could be written so that it executed instead of opening.**
  Every URL this app puts on the page goes through one function, `safeHref()`, which reads
  the scheme and refuses anything that is not `http`, `https`, `mailto` or `tel`. It read the
  scheme off the string as typed, and the browser reads it off something else: the URL parser
  removes tab, newline and carriage return from anywhere in a URL, and strips control
  characters from the front of one, before deciding what the scheme is. So `java<TAB>script:`
  matched no scheme at all, was waved through as "some kind of path", and became
  `javascript:` the moment it was written into the page. A link beginning with an invisible
  NUL did the same. Nothing was executing in practice — the Content-Security-Policy this app
  ships with has no `script-src` and no `unsafe-inline`, and it blocks a `javascript:` link
  outright — but a single policy header is not what the guardrail was supposed to be, and a
  household member is not a stranger you have promised nothing to. `safeHref()` now removes
  those characters *and* hands on what it removed them from, which is the half that matters:
  checking a cleaned string and then returning the dirty one leaves the browser reading the
  same URL it always did. One consequence a family may notice: a space inside a link is
  removed along with the control characters, so a link that was pasted with one in it needs
  the space taken out or written as `%20`.
  The server no longer stores such a link either. It already refused anything that did not
  begin with `http://` or `https://` — which is why nothing here was reachable in practice —
  and it now also refuses a link holding a tab or a line break, because a link holding one
  is not the link printed on the screen. Existing calendars are unaffected in the way that
  matters: the rule runs when an event is saved and nowhere else, so an event stored by 0.2.0
  with a tab in its link still lists, still opens and still sends its reminders. Editing that
  one event is refused until the stray character is taken out of the link box, which is the
  price of the check and is said in those words rather than as a failure to save.
  ([#20](https://github.com/d-weber/almanack/issues/20))
- **The unit, the timer and `/healthz` now behave the way the deployment contract says they
  do.** Four operational faults, none of them visible from a browser and all of them visible
  on the morning something goes wrong. Two backups running at once destroyed each other: the
  sweep that clears partial files from an interrupted run took every one it found, whatever
  its age, so a snapshot of a large database overtaken by the next hourly run had its file
  deleted underneath it — and what followed was worse than a lost file, because `VACUUM INTO`
  went on writing to the unlinked inode while the verification step opened the path afresh,
  created an empty database there, and failed it at the schema check. The household saw a
  "your backups are failing" mail and a 503 from `/healthz` until the next hour, about a
  backup that was never in trouble. Partial files are now left alone for an hour and carry
  the writing process's id, so two runs cannot meet at all; the hour is also why the off-host
  sync must keep skipping `*.tmp`, as it always should have. Snapshots are written `0600`
  rather than under whatever umask the timer's unit happened to have — the file is a complete
  copy of the calendar, every address and every password hash, and an off-host sync preserves
  the mode it finds. The systemd watchdog was a trap for anyone who tuned the scheduler: the
  ping was throttled to once per half of `WatchdogSec`, but it is only reached when a tick
  completes, so the real spacing was that half *plus* a whole tick against a deadline of the
  whole `WatchdogSec` — 30 seconds of margin at the defaults, and a restart loop for whoever
  set `ALMANACK_TICK=90s` to reduce load on a small box. The throttle is gone: a datagram
  costs nothing, the spacing is now exactly one tick, and the process warns at startup if the
  two settings are close enough to matter. Readiness was signalled before anything had bound
  the port, so `systemctl restart` returned success — and the install guide's "confirm the
  readiness signal arrived" passed — on a service that was about to die because a second copy
  of the unit, or a proxy, already held the address; the listener is opened before `READY=1`
  now, making the order migrations, bind, ready, serve. And `/healthz` reported a field named
  `database_exists` that only checked the configuration string was not empty, while the
  connection pool went on answering pings from a file that had been unlinked or a volume that
  never mounted, so a server whose calendar had gone reported itself healthy indefinitely: it
  stats the path now, and a database that is not there degrades the server.
  ([#23](https://github.com/d-weber/almanack/issues/23))
- **Running the browser tests twice failed the second time, for reasons that pointed
  nowhere near the cause.** Two of the smoke tests created an event and never removed it, so
  a second `make e2e` against the same seeded database found three other tests failing — the
  offline one, the cache-cap one and the timezone one — because an extra event changes what
  is on the screen and how many API ranges get cached. Nothing said "there is a leftover
  event"; it looked like a broken application. CI never saw it, since that job seeds a
  database before every run, so the whole cost fell on whoever ran the suite locally, which
  is the case the target exists for. Both tests now delete what they created, in a `finally`
  so that a failing assertion still tidies up, and through the API rather than by driving the
  delete flow in a test that is about something else. On top of that the suite now looks, once
  per run, for a fixture left behind by a run that was interrupted, and stops with the answer —
  run `make seed` — instead of letting three unrelated-looking tests go red.
  ([#52](https://github.com/d-weber/almanack/issues/52))
- **A failed backup could leave an empty calendar where the family's used to be.** Every
  run records its outcome where `/healthz` can read it, and that breadcrumb is written by
  opening the database — which creates and fully migrates the file when it is not there. So
  a backup taken while the data volume had failed to mount did the right thing twice over,
  refusing to back up nothing and exiting non-zero, and then left a fresh, empty, 217 KB
  Almanack database sitting at the data path. The next start came up on it without
  complaint, the health check went green, and the real calendar was still on the volume
  nobody had noticed was missing — which is a bad way to discover that the hourly timer had
  been running against an unmounted disk since the reboot. The outcome is now recorded only
  when there is a database to record it against; when there is not, the non-zero exit and
  the failure mail are the whole signal, as the deployment contract has always said.
  ([#29](https://github.com/d-weber/almanack/issues/29))
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
