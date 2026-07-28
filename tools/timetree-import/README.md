# TimeTree import

Turns the raw JSON `tools/timetree-export` produced into Almanack events, through the
public HTTP API. It is step 2 of
[docs/migrating-from-timetree.md](../../docs/migrating-from-timetree.md), which is its
specification and explains every mapping decision below.

Standard library only, Python 3.10+. Nothing to install.

Unlike the export, this is in no hurry: it reads files already on your disk, it can be
re-run as many times as it takes, and `--dry-run` shows you exactly what it would do
without touching the server.

## Do these first, in this order

Identities have to exist before the events that name them, and there is no way to fix
that afterwards without editing every event by hand.

1. Create the target calendar in Almanack.
2. Invite everybody, and wait until they have all signed up. An Almanack account is
   only ever created through an invite, so this is not a step the importer can do for
   you.
3. Write down the map from TimeTree user ids to Almanack ones. `calendar.json` in the
   export names the TimeTree members; `GET /api/v1/me` (or the dev dashboard) gives the
   Almanack ids. That map is `--user-map`.

Get the map wrong and events import with people missing from them. The importer prints
every TimeTree attendee id it could not map, with a count of the events it dropped them
from, so a dry run tells you whether the map is complete before anything is written.

## Running it

```sh
python3 tools/timetree-import/import.py \
    --base-url http://127.0.0.1:8080 \
    --email you@example.org --password-file ~/.almanack-pass \
    --calendar Famille \
    --source ~/timetree-raw/calendar-12345678 \
    --journal ~/timetree-raw/import-journal.jsonl \
    --user-map 62200160=1,62201842=2 \
    --dry-run
```

Drop `--dry-run` when the plan looks right. The password comes from a file rather than
from `argv`, where it would be visible to every other process on the machine and in your
shell history.

`--journal` is the file that makes re-running safe: one line per completed action, keyed
on the TimeTree `uuid`. A run interrupted halfway — a laptop closing, a network drop —
is resumed rather than duplicated, and re-running a finished import creates nothing.
Keep it beside the export; delete it only if you have also deleted what it records.

`--only-authors` imports just the events one TimeTree member wrote. Almanack sets
`created_by` from whoever is logged in and offers no way to say otherwise, so preserving
authorship means one pass per person, each signed in as themselves. If you do not care
who created what, one pass as anybody is fine.

## What it does and does not carry

Everything below is the mapping table in the migration document, applied. The parts
worth knowing without reading it:

- **Birthdays** (TimeTree `type: 1`) become yearly all-day events, which is the nearest
  thing Almanack has. Both halves are forced: some exports carry `all_day: false` on
  them, and a birthday imported as a timed one-off is a birthday that happens once.
- **Memos** (`category: 2`) are skipped. They have no start and nothing to become; if
  you have a handful, retyping them beats writing code for them.
- **Recurrence** is yearly only, which is all a TimeTree export has been seen to
  contain. Anything else stops the run before it writes, rather than importing most of
  a series and leaving you to find the rest. An `EXDATE` becomes a single cancelled
  occurrence once its series exists.
- **Reminders** are imported only for events in the future or ones that recur —
  otherwise the first planning pass would announce a decade of history at once. They
  attach to the account running the import, because that is what the API supports.
- **`created_at` / `updated_at` do not survive.** The server stamps both from its own
  clock when it creates a row and offers no way to say otherwise, so every imported
  event reads as created on the day of the import. The mapping table asks for the audit
  trail and this is the one row of it that cannot be honoured over HTTP; the raw export
  keeps it if you ever need it.
- **Locations, URLs, notes, labels and participants** all carry across directly.
  Geography (`location_lat`/`location_lon`), attachments and TimeTree's per-event chat
  do not — Almanack stores none of them.

## When it stops

It is deliberately loud. A recurrence rule it does not fully understand, a `label_id`
outside the ten a calendar has, an all-day alert that does not convert to a wall-clock
time — each aborts the run with the event named, before anything is written. The
alternative is a half-imported calendar whose gaps nobody notices for a month.

The one thing it reports rather than refuses is an unmapped attendee, because a
household that has genuinely lost a member since the export is a normal thing to
migrate, and refusing it would leave you no way through. Read the warning at the end of
a dry run before committing to a real one.
