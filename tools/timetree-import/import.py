#!/usr/bin/env python3
"""Import a TimeTree calendar (as exported by tools/timetree-export) into a
running Almanack instance, through the public HTTP API.

Stdlib only, like the exporter. See docs/migrating-from-timetree.md for the
field mapping this implements.

Design points:
  - Idempotent via a local journal (JSONL, one line per completed action,
    keyed on the TimeTree uuid). Re-running skips everything already done,
    so a run interrupted halfway is resumed, not duplicated.
  - --dry-run prints the full plan and mutates nothing.
  - Events are created by whoever is logged in; Almanack assigns created_by
    server-side. --only-authors permits a per-author pass (log in as each
    person and import only their events).
  - Imports TimeTree plans (category 1) of both kinds: ordinary events and
    birthdays (type 1), the latter as the yearly all-day events the mapping
    calls for, since Almanack has no birthday type. Memos (category 2) are
    skipped — they have no start and nothing to become. Yearly RRULEs only;
    anything else aborts loudly before touching the server.
  - What cannot survive the public API: created_at / updated_at. The server
    stamps both from its own clock when it creates the row, and offers no way
    to say otherwise, so the audit trail the mapping table asks for arrives as
    the date of the import rather than the date of the change. The raw export
    keeps it; this is a limit of importing over HTTP rather than a choice.
  - Reminders are imported only for future or recurring events, to avoid a
    notification flood over historical data. They attach to the logged-in
    user only (that is the API's semantics).

Usage:
  python3 import.py --base-url http://127.0.0.1:8080 \
      --email you@example.org --password-file ~/.almanack-pass \
      --calendar Famille \
      --source ~/timetree-raw/calendar-12345678 \
      --journal ~/timetree-raw/import-journal.jsonl \
      --user-map 62200160=1,62201842=2 \
      --dry-run

See README.md beside this file for the order to do things in, which matters:
identities have to exist in Almanack before the events that name them.
"""

import argparse
import datetime as dt
import http.cookiejar
import json
import re
import sys
import urllib.error
import urllib.request

UTC = dt.timezone.utc


def die(msg: str) -> None:
    print(f"FATAL: {msg}", file=sys.stderr)
    sys.exit(1)


class Api:
    def __init__(self, base_url: str):
        self.base = base_url.rstrip("/")
        jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(jar)
        )

    def call(self, method: str, path: str, body=None):
        req = urllib.request.Request(self.base + path, method=method)
        if method != "GET":
            req.add_header("X-Requested-With", "almanack")
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            req.add_header("Content-Type", "application/json")
        try:
            with self.opener.open(req, data) as resp:
                raw = resp.read()
                return resp.status, json.loads(raw) if raw else None
        except urllib.error.HTTPError as e:
            detail = e.read().decode(errors="replace")[:400]
            raise RuntimeError(f"{method} {path} -> {e.code}: {detail}") from e


# --- conversions -------------------------------------------------------------

def iso_utc(ms: int) -> str:
    return dt.datetime.fromtimestamp(ms / 1000, UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def utc_date(ms: int) -> dt.date:
    return dt.datetime.fromtimestamp(ms / 1000, UTC).date()


def all_day_dates(e: dict) -> tuple[str, str]:
    """TimeTree end_at is EXCLUSIVE, Almanack end_date is INCLUSIVE: minus one
    day, clamped for the zero-length events TimeTree also produces."""
    start = utc_date(e["start_at"])
    end = max(start, utc_date(e["end_at"]) - dt.timedelta(days=1))
    return start.isoformat(), end.isoformat()


RRULE_YEARLY = re.compile(r"^RRULE:FREQ=YEARLY(;WKST=[A-Z]{2})?$")
EXDATE_RE = re.compile(r"^EXDATE:(\d{8})T\d{6}Z?$")


def parse_recurrence(e: dict) -> tuple[dict | None, list[str]]:
    """Returns (recurrence payload, exdates as YYYY-MM-DD). Yearly only; a rule
    this code does not fully understand aborts the run rather than half-importing."""
    rec, exdates = None, []
    for r in e.get("recurrences") or []:
        if RRULE_YEARLY.match(r):
            rec = {"freq": "yearly", "interval": 1}
        elif m := EXDATE_RE.match(r):
            d = m.group(1)
            exdates.append(f"{d[0:4]}-{d[4:6]}-{d[6:8]}")
        else:
            die(f"unsupported recurrence {r!r} on {e['title']!r} ({e['uuid']})")
    if exdates and not rec:
        die(f"EXDATE without RRULE on {e['title']!r}")
    return rec, exdates


def reminders_for(e: dict, future_or_recurring: bool) -> list[dict]:
    if not future_or_recurring:
        return []
    out = []
    for m in e.get("alerts") or []:
        if e["all_day"]:
            # m = minutes before midnight; 900 -> 1 day before at 09:00
            days = max(0, -(-m // 1440)) if m > 0 else 0
            at = days * 1440 - m
            if at < 0 or at >= 1440:
                die(f"unconvertible all-day alert {m} on {e['title']!r}")
            out.append({"days_before": days,
                        "at_time_local": f"{at // 60:02d}:{at % 60:02d}"})
        else:
            if m < 0:
                continue  # "after start" has no Almanack equivalent
            out.append({"offset_minutes": m})
    return out


def label_for(e, label_ids):
    """TimeTree label ids are 1-based positions into the calendar's ten labels.

    Checked rather than indexed straight, because Python's negative indexing turns the
    one value that should be impossible — 0 — into the *last* label rather than an
    error: every such event would import, look right, and be filed under the wrong
    colour with nothing to notice."""
    n = e.get("label_id")
    if not isinstance(n, int) or not 1 <= n <= len(label_ids):
        die(f"label_id {n!r} on {e.get('title')!r} ({e.get('uuid')}) is outside the "
            f"1..{len(label_ids)} the calendar has")
    return label_ids[n - 1]


def build_payload(e, cal_id, label_ids, user_map, today, unmapped):
    rec, exdates = parse_recurrence(e)
    # A TimeTree birthday carries its recurrence in its type rather than in an RRULE:
    # it is the one kind whose yearly repeat is implied. docs/migrating-from-timetree.md
    # maps it to a yearly all-day event, which is the nearest thing Almanack has, and
    # both halves have to be forced here — some exports carry all_day false on them, and
    # a birthday imported as a timed one-off is a birthday that happens once.
    birthday = e.get("type") == 1
    if birthday:
        rec = rec or {"freq": "yearly", "interval": 1}
    payload = {
        "calendar_id": cal_id,
        "title": (e.get("title") or "").strip() or "(sans titre)",
        "all_day": bool(e["all_day"]) or birthday,
        "location": e.get("location") or "",
        "url": e.get("url") or "",
        "notes": e.get("note") or "",
        "label_id": label_for(e, label_ids),
        "participants": sorted({user_map[a] for a in e.get("attendees") or []
                                if a in user_map}),
    }
    # An attendee the map does not name is dropped from the event, and a dropped
    # participant is invisible afterwards: the event imports, looks complete, and is
    # simply missing somebody. One mistyped id in --user-map does that to every event
    # that person was on, so the ids are collected and reported rather than passed over.
    for a in e.get("attendees") or []:
        if a not in user_map:
            unmapped[a] = unmapped.get(a, 0) + 1
    if payload["all_day"]:
        payload["start_date"], payload["end_date"] = all_day_dates(e)
    else:
        payload["starts_at"] = iso_utc(e["start_at"])
        payload["ends_at"] = iso_utc(e["end_at"])
    if rec:
        payload["recurrence"] = rec
    future = rec is not None or utc_date(e["start_at"]) >= today
    rem = reminders_for(e, future)
    if rem:
        payload["reminders"] = rem
    return payload, exdates


# --- main ----------------------------------------------------------------------

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", required=True)
    ap.add_argument("--email", required=True)
    ap.add_argument("--password-file", required=True,
                    help="file holding the password; never passed on argv")
    ap.add_argument("--calendar", help="target calendar name (exact match)")
    ap.add_argument("--calendar-id", type=int)
    ap.add_argument("--source", required=True,
                    help="calendar-<id>/ directory from the exporter")
    ap.add_argument("--journal", required=True)
    ap.add_argument("--user-map", default="",
                    help="ttid=almanackid[,ttid=almanackid...] for participants")
    ap.add_argument("--only-authors", default="",
                    help="import only events whose TimeTree author_id is listed")
    ap.add_argument("--today", default=None,
                    help="override the future-event cutoff date (for rehearsal)")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    today = dt.date.fromisoformat(args.today) if args.today else dt.date.today()
    user_map = {}
    if args.user_map:
        for pair in args.user_map.split(","):
            k, v = pair.split("=")
            user_map[int(k)] = int(v)
    only_authors = {int(x) for x in args.only_authors.split(",") if x}

    with open(f"{args.source}/events.json") as f:
        raw = json.load(f)
    events = raw if isinstance(raw, list) else raw["events"]

    # Journal of completed actions; presence of a key means "done".
    done = set()
    try:
        with open(args.journal) as f:
            for line in f:
                done.add(json.loads(line)["key"])
    except FileNotFoundError:
        pass

    api = Api(args.base_url)
    password = open(args.password_file).read().strip()
    api.call("POST", "/api/v1/auth/login",
             {"email": args.email, "password": password})

    _, me = api.call("GET", "/api/v1/me")
    cals = me["calendars"]
    if args.calendar_id:
        cal = next((c for c in cals if c["id"] == args.calendar_id), None)
    else:
        if not args.calendar:
            die("need --calendar or --calendar-id")
        cal = next((c for c in cals if c["name"] == args.calendar), None)
    if not cal:
        die(f"calendar not found; available: {[c['name'] for c in cals]}")
    labels = sorted(cal["labels"], key=lambda l: l["position"])
    if len(labels) < 10:
        die(f"expected 10 labels, calendar has {len(labels)}")
    label_ids = [l["id"] for l in labels]
    print(f"target: calendar {cal['id']} {cal['name']!r}")
    print(f"members: {[(m['user_id'], m['display_name']) for m in cal['members']]}")
    print(f"user map (timetree -> almanack): {user_map or 'NONE — participants dropped'}")

    # Plans (category 1) of both kinds the spec maps: ordinary events and birthdays.
    # Memos (category 2) have no start and nothing to become.
    todo = [e for e in events if e.get("category") == 1 and e.get("type") in (0, 1)]
    skipped_kind = len(events) - len(todo)
    birthdays = sum(1 for e in todo if e.get("type") == 1)
    if only_authors:
        todo = [e for e in todo if e["author_id"] in only_authors]
    todo.sort(key=lambda e: e["start_at"])

    created = skipped_journal = 0
    plan_exdates = []
    # TimeTree attendee ids seen on some event but absent from --user-map, and how
    # many events each was dropped from. Reported at the end; see build_payload.
    unmapped_attendees: dict[int, int] = {}
    for e in todo:
        key = e["uuid"]
        payload, exdates = build_payload(e, cal["id"], label_ids, user_map, today,
                                         unmapped_attendees)
        for d in exdates:
            plan_exdates.append((key, e["title"], d))
        if key in done:
            skipped_journal += 1
            continue
        if args.dry_run:
            created += 1
            continue
        _, resp = api.call("POST", "/api/v1/events", payload)
        event_id = resp["event"]["id"]
        with open(args.journal, "a") as f:
            f.write(json.dumps({"key": key, "event_id": event_id,
                                "title": payload["title"]}) + "\n")
        done.add(key)
        # remember the id so the exdate step below can find its series
        e["_almanack_id"] = event_id
        created += 1

    # EXDATE -> cancel that single occurrence of the series (scope=this).
    ex_done = ex_todo = 0
    for uuid, title, date in plan_exdates:
        key = f"{uuid}:exdate:{date}"
        if key in done:
            ex_done += 1
            continue
        ex_todo += 1
        if args.dry_run:
            continue
        event_id = next((json.loads(l)["event_id"] for l in open(args.journal)
                         if json.loads(l)["key"] == uuid), None)
        if event_id is None:
            die(f"series {title!r} not in journal; cannot apply EXDATE")
        api.call("DELETE", f"/api/v1/events/{event_id}?scope=this&date={date}")
        with open(args.journal, "a") as f:
            f.write(json.dumps({"key": key, "event_id": event_id}) + "\n")

    mode = "DRY-RUN: would create" if args.dry_run else "created"
    print(f"{mode} {created} events "
          f"({skipped_journal} already in journal, {birthdays} of them birthdays, "
          f"{skipped_kind} memos skipped, "
          f"{len(events) - skipped_kind - len(todo)} filtered by author)")
    print(f"exdate occurrence deletions: {ex_todo} to do, {ex_done} already done")

    # Said last, and to stderr, because it is the one outcome of a successful run that
    # needs acting on: everything else here is a count, and this is data that did not
    # arrive. A dry run reports it too, which is where it is cheapest to notice.
    if unmapped_attendees:
        print("\nWARNING: these TimeTree attendee ids are not in --user-map, so they "
              "were left off every event they were on:", file=sys.stderr)
        for tt_id, n in sorted(unmapped_attendees.items(), key=lambda kv: -kv[1]):
            print(f"  {tt_id}: dropped from {n} event(s)", file=sys.stderr)
        print("Add them to --user-map and re-run: the journal skips what is already "
              "imported, but participants are set when an event is created, so events "
              "already imported keep the list they were created with.", file=sys.stderr)


if __name__ == "__main__":
    main()
