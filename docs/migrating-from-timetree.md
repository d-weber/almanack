# Migrating from TimeTree

TimeTree has [no export feature](https://support.timetreeapp.com/hc/en-us/articles/205954655-Can-you-export-events-and-sync-TimeTree-to-OS-calendars-Google-Calendar-etc-)
— officially, "you cannot export entries and events to another calendar at this time."
The only way out is the private API the TimeTree web app uses.

Migration is two independent jobs, and they should stay independent:

1. **Get the data out**, verbatim and completely, while the account still exists.
   `tools/timetree-export/export.py` does this. **Done — see below.**
2. **Turn it into Almanack rows.** `tools/timetree-import/import.py` does this, and
   this document is what it implements. **Done — see
   [its README](../tools/timetree-import/README.md) for the order to do things in.**

Step 1 had the deadline: it depends on an undocumented API belonging to someone else,
which can change without notice. Step 2 reads files on your disk and can be redone as
many times as it takes.

## What the export produces

The exporter writes one directory per calendar plus a manifest, and prints a summary
when it finishes. On the account this was developed against — two calendars, ~500
events spanning four decades — the run took a few seconds and produced a couple of
megabytes of JSON.

Findings from that run that are worth knowing before writing an importer, and which
you should re-check against your own export rather than assume:

- **Attachments and photos may not exist at all.** Every event had an empty `files`
  array, `media_content_count: 0`, and `attachment` as the stub
  `{"virtual_user_attendees": []}`. If that holds for you, Almanack's decision to leave
  photos out costs nothing in migration.
- **No tombstones.** No event carried a `deactivated_at`, so there was nothing to
  filter out.
- **`uuid` is unique per event**, which gives an importer a free idempotency key —
  re-running an import cannot duplicate events.
- The mix skewed heavily to timed events over all-day ones, with attendees on almost
  every event and alerts on most. Labels and attendees are the fields worth protecting
  in any intermediate format (see below).

## Why not iCalendar

The community tool [TimeTree-Exporter](https://github.com/eoleedi/TimeTree-Exporter)
converts TimeTree to `.ics`, which is the right answer for moving to Google Calendar.
It is the wrong intermediate format here, because the fields it must drop are the ones
Almanack most wants: **labels** and **participants**, and in practice nearly every event
carries attendees. Raw JSON keeps them.

It remains useful as a cross-check: exporting the same calendar both ways and
comparing event counts is a cheap way to catch a pagination bug.

## Running the export

Nothing to install — standard library only, Python 3.10+.

The reliable route borrows a session from a browser already logged in, sidestepping
sign-in entirely. The API rejects programmatic sign-in with a generic `-702` even for
accounts that do have a working email password, so this is the supported path, not a
fallback. Copy the `_session_id` cookie for `timetreeapp.com` from Chrome DevTools →
Application → Cookies:

```sh
python3 tools/timetree-export/export.py --session-id 'PASTE_HERE' --out ./timetree-raw
```

Full instructions, the `--debug` flag and the error-code table are in
[tools/timetree-export/README.md](../tools/timetree-export/README.md).

Output layout:

```
timetree-raw/
  calendars.json              every calendar the account can see
  manifest.json               id, name, event count, label count per calendar
  calendar-<id>/
    calendar.json             that calendar's metadata, including its members
    labels.json               label names and colours
    events.json               every event, all pages merged
```

## Mapping to Almanack

The two models line up unusually well — Almanack was designed after TimeTree's mechanics,
and the ten-labels-per-calendar rule ([`domain.LabelsPerCalendar`](../internal/domain/domain.go))
came from there. The export confirms it: both calendars have exactly ten labels, ids
`1`–`10`.

| TimeTree | Almanack | Notes |
|---|---|---|
| calendar | `Calendar` | name, colour |
| `calendar_users` | `Member` + `User` | 7 distinct people; needs an identity map |
| `calendar_labels` | `Label` | ids 1–10 → positions 1–10; colour is an **int**, format as `#%06x` |
| `title` / `note` / `location` / `url` | `Title` / `Notes` / `Location` / `URL` | direct |
| `location_lat` / `location_lon` | — | dropped; Almanack stores no geography |
| `start_at` / `end_at` | `StartsAt`/`EndsAt` or `StartDate`/`EndDate` | epoch **milliseconds**; see below |
| `all_day` | `AllDay` | selects which pair above is populated |
| `label_id` | `LabelID` | per-calendar remap |
| `attendees` | `Participants` | array of TimeTree user ids |
| `author_id` | `CreatedBy` | |
| `alerts` | `Reminder` | minutes, signed; see below |
| `recurrences` | `Recurrence` | RRULE strings; see below |
| `created_at` / `updated_at` | `CreatedAt` / `UpdatedAt` | **not carried** — see below |
| `uuid` | — | keep as the import key, for idempotent re-runs |
| `type: 1` (birthday) | yearly all-day event | 3 events; Almanack has no birthday type |
| `category: 2` (memo) | — | exactly 1 event; see below |
| `attachment`, `files`, `lunar`, `pinned_at`, `like_count`, `row_order` | — | unused or empty throughout |

### The parts that need care

All of this is now confirmed against the actual dump, not inferred.

**All-day events, and the off-by-one.** Every all-day event has
`start_timezone: "UTC"` — TimeTree normalises them, and the correlation is exact (103
all-day events, 103 UTC events). So the "all-day event authored in another timezone"
hazard is **zero occurrences**; no heuristic needed.

`end_at` is **exclusive**: a single-day all-day event has `end_at - start_at` of
exactly one day. Almanack's `EndDate` is **inclusive**, so:

```
StartDate = utc_date(start_at)
EndDate   = utc_date(end_at) - 1 day
```

Getting this backwards puts every multi-day event a day wrong. This is finding C6 of
the design review, arriving through the front door —
and [`domain.Date`](../internal/domain/date.go) is what makes the corrected form
representable.

**Timed events** carry real zones: Europe/Paris (389), plus Asia/Tokyo (4),
Asia/Bangkok (2), Africa/Nairobi (1) — holidays, presumably. These convert cleanly to
UTC instants with no ambiguity, since an instant is an instant.

**Recurrence is trivial here.** Only 6 events recur, and there are exactly three
distinct rules in the entire export:

```
RRULE:FREQ=YEARLY
RRULE:FREQ=YEARLY;WKST=MO
RRULE:FREQ=YEARLY  +  EXDATE:20240705T000000Z
```

All yearly, i.e. birthdays and anniversaries. `WKST` is meaningless for a yearly rule
and can be ignored. So Almanack needs no general RRULE parser (technical plan §5 says it
ships none): handle `FREQ=YEARLY` and reject everything else loudly. The single
`EXDATE` maps to deleting that one occurrence with `ScopeThis` after the series is
created — one manual step, not a feature.

**Alerts are signed minutes relative to start**, and the sign is the interesting part.
The distinct values are `10`, `50`, `120`, `900` and `-540`. Positive is *before*
start. The two that look odd both decode cleanly on all-day events, where start is
local midnight:

- `900` → 900 minutes before midnight → **09:00 the previous day** → `DaysBefore: 1`, `AtTimeLocal: "09:00"`
- `-540` → 540 minutes *after* midnight → **09:00 on the day** → `DaysBefore: 0`, `AtTimeLocal: "09:00"`

Which is exactly why [`domain.Reminder`](../internal/domain/domain.go) splits into
`OffsetMinutes` for timed events and `DaysBefore` + `AtTimeLocal` for all-day ones.
Timed events use the first form directly (`10` → 10 minutes before).

The remaining mismatch is ownership: a TimeTree alert belongs to the *event*, an Almanack
reminder belongs to *one person*. Fanning 345 alerts out to every member would produce
a wall of notifications on first boot — import alerts only for events in the future.

**Labels need no thought.** Every label name is empty in both calendars (nobody ever
renamed one) and the colours are TimeTree's stock palette. Map `label_id` 1–10 to
positions 1–10, carry the colours across as `#%06x`, done.

**Identity is the one real prerequisite.** TimeTree user ids mean nothing to Almanack,
and Almanack accounts are created only through invites. Order of operations: create the
calendars, invite the family, let everyone sign up, *then* run the importer with a
hand-written map of the seven ids. `CreatedBy` and `Participants` both depend on it.

**Idempotency.** Give the importer a dry-run mode and key inserts on the TimeTree
`uuid`, so a re-run after a fix does not duplicate the family's history. The first run
will be wrong in some small way. Both are built: `--dry-run`, and a `--journal` of
completed actions keyed on the `uuid`, so an interrupted run resumes rather than
duplicating.

**The audit trail is the one row of the table above that cannot be honoured.** The API
stamps `created_at` and `updated_at` from the server's clock when it creates a row and
offers no way to say otherwise, so every imported event reads as created on the day of
the import. Writing them would mean importing through the store rather than over HTTP —
a second, privileged path into the database, for a field nothing in the application
displays. The raw export keeps the real values if they are ever wanted.

### Open decisions

- **Memos** (`category: 2`). TimeTree's undated notes have no equivalent in Almanack. If
  there are only a handful, retyping them by hand beats writing code for them.
- **How far back to import.** Exports often reach surprisingly far into the past,
  usually because a recurring birthday is anchored at someone's actual birth year
  rather than because there is decades of real history. At a few hundred events the
  volume is irrelevant either way; a cutoff mainly saves you mapping identities for
  members who have since left the calendar.
- **Comments.** TimeTree's per-event chat is out of scope for Almanack (see
  [architecture.md](architecture.md)) and the export tool does not fetch it. Keeping
  any would need a separate pass — one request per event — and they would have to land
  in an event's notes.
