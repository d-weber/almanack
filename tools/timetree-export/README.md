# TimeTree export

Dumps every TimeTree calendar an account can see, as raw JSON, using the private API
the TimeTree web app uses. TimeTree has no export feature; this is the only way out.

Standard library only, Python 3.10+. Nothing to install. What to do with the output is
[docs/timetree-migration.md](../../docs/timetree-migration.md).

## Preferred: borrow a browser session

This skips the password entirely, which matters because TimeTree accounts created with
Google or Apple sign-in have no email password at all — and the API rejects those with
the same error as a wrong password, so a password you are certain of can still fail.

1. Open <https://timetreeapp.com> in Chrome and make sure you are logged in.
2. DevTools (`F12`) → **Application** → **Storage** → **Cookies** →
   `https://timetreeapp.com`.
3. Copy the **Value** of the `_session_id` row. The value only — no name, no quotes, no
   trailing semicolon.

```sh
python3 tools/timetree-export/export.py --session-id 'PASTE_HERE' --out ./timetree-raw
```

The cookie is a live credential for your account, so prefer the env var if you would
rather it stayed out of your shell history:

```sh
read -rs TIMETREE_SESSION_ID && export TIMETREE_SESSION_ID
python3 tools/timetree-export/export.py --out ./timetree-raw
```

Sessions expire. If the export dies partway with `-401`, copy a fresh cookie and rerun
— the script rewrites everything from scratch, so a rerun is always safe.

## Alternative: email and password

Works only for accounts with a real email password.

```sh
python3 tools/timetree-export/export.py --email you@example.org --out ./timetree-raw
```

The password is prompted for (or read from `TIMETREE_PASSWORD`), sent to
timetreeapp.com over HTTPS, and never written to disk.

## Output

```
timetree-raw/
  calendars.json              every calendar the account can see
  manifest.json               id, name, event count, label count per calendar
  calendar-<id>/
    calendar.json             that calendar's metadata, including its members
    labels.json               label names and colours
    events.json               every event, all pages merged
```

Check `manifest.json` before relying on it: counts should look right for each calendar,
and a calendar you know is busy must not come back with zero. The script warns on
stderr if it stopped at its page cap.

## When it breaks

This talks to an undocumented API belonging to someone else, and it will eventually
stop working. The error codes seen so far:

| Code | Meaning |
|---|---|
| `-702` | Credentials rejected — wrong password, **or** an account with no email password |
| `-495` | Sign-in rate limited; wait a few minutes |
| `-401` | Session cookie missing, wrong or expired |
| `-493` | Request rejected outright — no session sent, or the API changed |

If `X-Timetreea: web/2.1.0/en` (in `export.py`) is ever retired, that header is the
first thing to update; the current value is visible in the Network tab of the TimeTree
web app. [TimeTree-Exporter](https://github.com/eoleedi/TimeTree-Exporter) is an
actively maintained implementation of the same API and is worth diffing against.
