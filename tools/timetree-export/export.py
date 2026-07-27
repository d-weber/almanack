#!/usr/bin/env python3
"""Dump every TimeTree calendar this account can see, as raw JSON.

TimeTree has no export feature. This talks to the same private web API the
TimeTree web app uses, and writes what it returns to disk unmodified: no
conversion, no field mapping, no interpretation. The point is to get the data
out once, completely, while the account still exists. Turning it into Almanack
rows is a separate job (docs/migrating-from-timetree.md) that reads these files.

Standard library only, so there is nothing to install and nothing to rot.

    python3 export.py --email you@example.org --out ./timetree-raw

The password is read interactively, or from TIMETREE_PASSWORD. It is sent to
timetreeapp.com over HTTPS and to nowhere else; only the returned session
cookie is kept, in memory, for the life of the process.
"""

import argparse
import getpass
import http.cookiejar
import json
import os
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

API = "https://timetreeapp.com/api/v1"

# The web app identifies itself with this header on every call; requests without
# it are rejected. It is not a version we control — if TimeTree ships a new web
# client and retires this one, this is the first thing to update.
USER_AGENT = "web/2.1.0/en"

# A calendar with more events than this is not expected; the cap only exists so
# that a malformed `chunk` flag cannot spin forever.
MAX_PAGES = 500

# urllib announces itself as "Python-urllib/3.x", which is a string plenty of
# edge proxies drop on sight. Everything here exists to look like the web client
# whose API this is, rather than to be clever.
BROWSER_HEADERS = {
    "User-Agent": (
        "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) "
        "Chrome/140.0.0.0 Safari/537.36"
    ),
    "Accept": "application/json, text/plain, */*",
    "Accept-Language": "en-US,en;q=0.9",
    "Accept-Encoding": "identity",  # claim only what we decode
    "Origin": "https://timetreeapp.com",
    "Referer": "https://timetreeapp.com/signin",
}

# Set by --debug: print each request and the server's unedited reply.
DEBUG = False


class TimeTreeError(Exception):
    """Anything the API refused to do."""


def _opener(session_id=None):
    """An opener that keeps cookies, which is how the session is carried.

    With a session_id it starts already authenticated, borrowed from a browser
    that is logged in; `login()` is then unnecessary. That is the escape hatch
    for accounts with no email password (Google/Apple sign-in) and for any
    future change to the sign-in flow, which is someone else's to make.
    """
    opener = urllib.request.build_opener(
        urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar())
    )
    if session_id:
        opener.addheaders = [("Cookie", f"_session_id={session_id}")]
    return opener


def _call(opener, method, path, body=None):
    """One API call. Returns parsed JSON, raises TimeTreeError on refusal."""
    data = json.dumps(body).encode() if body is not None else None
    headers = dict(BROWSER_HEADERS)
    headers.update({"Content-Type": "application/json", "X-Timetreea": USER_AGENT})
    req = urllib.request.Request(f"{API}{path}", data=data, method=method, headers=headers)
    if DEBUG:
        redacted = {**body, "password": "***"} if body and "password" in body else body
        print(f"\n  --> {method} {API}{path}\n      body: {json.dumps(redacted)}", file=sys.stderr)
    try:
        with opener.open(req, timeout=30) as resp:
            raw = resp.read().decode()
            if DEBUG:
                print(f"      <-- {resp.status} {raw[:600]}", file=sys.stderr)
            return json.loads(raw)
    except urllib.error.HTTPError as e:
        raise TimeTreeError(_explain(e)) from e
    except urllib.error.URLError as e:
        raise TimeTreeError(f"could not reach timetreeapp.com: {e.reason}") from e


def _explain(e):
    """Turn an HTTP error into something worth reading."""
    try:
        raw = e.read().decode()
    except (AttributeError, ValueError):
        raw = ""
    if DEBUG:
        print(f"      <-- {e.code} {raw[:600]}", file=sys.stderr)
    try:
        err = json.loads(raw).get("error") or {}
        code = err.get("code")
    except (ValueError, AttributeError):
        code, err = None, {}
    if code == -702:
        return (
            "TimeTree rejected the sign-in (-702). Seen for a wrong password, for an "
            "account with no email password (Google/Apple sign-up), and for a correct "
            "password the API declines to accept from a non-browser client — this is an "
            "undocumented endpoint and the code is not specific. Do not keep retrying; "
            "use --session-id instead, which bypasses sign-in entirely and is the "
            "supported route here. See tools/timetree-export/README.md."
        )
    if code == -495:
        return "TimeTree is rate-limiting sign-ins. Wait a few minutes and retry."
    if code == -401:
        return (
            "the session cookie was not accepted (-401): wrong value, or expired. Copy "
            "_session_id again from a browser tab that is currently logged in — the value "
            "only, with no name, quotes or semicolon."
        )
    if code == -493:
        return "no session was sent (-493). Pass --session-id, or an email and password."
    return f"HTTP {e.code}: {err.get('message') or e.reason} (code {code})"


def login(opener, email, password):
    """Sign in. The session lands in the opener's cookie jar as a side effect."""
    _call(
        opener,
        "PUT",
        "/auth/email/signin",
        {"uid": email, "password": password, "uuid": uuid.uuid4().hex},
    )


def fetch_events(opener, calendar_id):
    """Every event in a calendar, following TimeTree's `chunk` pagination.

    The first page is requested without a cursor; each response says whether
    another chunk follows and where it starts. A cursor that fails to advance
    would loop forever, so it is treated as the end.
    """
    events, since, pages = [], None, 0
    while pages < MAX_PAGES:
        pages += 1
        suffix = "" if since is None else f"?since={since}"
        page = _call(opener, "GET", f"/calendar/{calendar_id}/events/sync{suffix}")
        events.extend(page.get("events") or [])
        if not page.get("chunk"):
            break
        nxt = page.get("since")
        if nxt is None or nxt == since:
            break
        since = nxt
    else:
        print(f"    ! stopped at {MAX_PAGES} pages — export may be incomplete", file=sys.stderr)
    return events


def _write(path, payload):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(payload, indent=2, ensure_ascii=False, sort_keys=True), encoding="utf-8"
    )


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--email", help="TimeTree account email (prompted if omitted)")
    ap.add_argument(
        "--session-id",
        help="_session_id cookie from a logged-in browser; skips the password entirely",
    )
    ap.add_argument("--out", default="./timetree-raw", help="output directory")
    ap.add_argument(
        "--debug", action="store_true", help="print each request and the server's raw reply"
    )
    args = ap.parse_args()

    global DEBUG
    DEBUG = args.debug

    session_id = args.session_id or os.environ.get("TIMETREE_SESSION_ID")
    out = Path(args.out)
    opener = _opener(session_id)

    try:
        if session_id:
            print("Using the session cookie supplied; not signing in.")
        else:
            email = args.email or input("TimeTree email: ").strip()
            password = os.environ.get("TIMETREE_PASSWORD") or getpass.getpass(
                "TimeTree password: "
            )
            if not email or not password:
                sys.exit("email and password are both required")
            print(f"Signing in as {email} ...")
            login(opener, email, password)

        calendars = _call(opener, "GET", "/calendars?since=0").get("calendars") or []
        _write(out / "calendars.json", calendars)
        print(f"Found {len(calendars)} calendar(s).")

        manifest = []
        for cal in calendars:
            cal_id, name = cal.get("id"), cal.get("name") or "(untitled)"
            if cal_id is None:
                continue
            print(f"  {name} (id {cal_id})")
            dest = out / f"calendar-{cal_id}"

            labels = _call(opener, "GET", f"/calendar/{cal_id}/labels")
            _write(dest / "labels.json", labels)

            events = fetch_events(opener, cal_id)
            _write(dest / "events.json", events)
            _write(dest / "calendar.json", cal)

            n_labels = len(labels.get("calendar_labels") or [])
            print(f"    {len(events)} events, {n_labels} labels")
            manifest.append(
                {"id": cal_id, "name": name, "events": len(events), "labels": n_labels}
            )
            time.sleep(0.5)  # be a polite guest on someone else's private API

        _write(out / "manifest.json", manifest)
    except TimeTreeError as e:
        sys.exit(f"\nExport failed: {e}")
    except KeyboardInterrupt:
        sys.exit("\nInterrupted.")
    except EOFError:
        sys.exit("\nNo credentials on a non-interactive run: pass --session-id.")

    total = sum(c["events"] for c in manifest)
    print(f"\nDone: {total} events across {len(manifest)} calendar(s) in {out}/")
    print("Keep this directory. It is the only copy of your TimeTree history.")


if __name__ == "__main__":
    main()
