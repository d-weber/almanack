# API contract (v1)

The agreement between `internal/httpapi` and `web/`. Both are built against this document,
so it is normative: if code and contract disagree, that is a bug in the code.

## Conventions

- Base path `/api/v1`. JSON in, JSON out, UTF-8. Changes within v1 are **additive only**.
- Every non-GET request must send `X-Requested-With: almanack` (CSRF defense) and
  `Content-Type: application/json` unless stated otherwise. **Mutations are never GET.**
- Auth is the `almanack_session` cookie (HttpOnly, SameSite=Lax, Secure outside dev).
- Every response carries `X-App-Version`. The client compares it with its own build hash and
  hard-reloads on mismatch.
- Errors: HTTP status + `{"error":{"code":"...","message":"..."}}`.
  Codes: `unauthorized`, `forbidden`, `not_found`, `invalid`, `conflict`, `rate_limited`, `internal`.
  Messages are for humans/logs; the client shows localized text keyed by `code`.
- Timed instants are RFC 3339 UTC (`2026-08-04T14:30:00Z`). Dates are `YYYY-MM-DD`.
  All-day events use dates only, `end_date` **inclusive**.

## Public (no session)

### `GET /api/v1/config`
Bootstrap for the login and join screens; safe to cache.
```json
{ "family_tz": "Europe/Paris", "app_version": "a1b2c3d4",
  "vapid_public_key": "BEl…", "languages": ["fr","en"], "dev_mode": false,
  "holiday_color": "#d32f2f" }
```

### `GET /api/v1/invites/{token}`
Preview an invite before signing up. Always 200 with `valid`, never leaking why.
```json
{ "valid": true, "calendar_name": "Family", "calendar_color": "#3b7ddd" }
```

### `POST /api/v1/auth/signup`
The only route to an account.
```json
{ "invite_token":"…", "email":"…", "password":"…", "display_name":"Mum", "color":"#c0392b", "lang":"en" }
```
→ `201` `{ "user": User }` + session cookie. `409 conflict` if the email exists.

### `POST /api/v1/auth/login`
`{ "email":"…", "password":"…" }` → `200 { "user": User }` + cookie.
`401 unauthorized` for both bad password and unknown email (no enumeration).
`429 rate_limited` after repeated failures from one address.

### `POST /api/v1/auth/password-reset/request`
`{ "email":"…" }` → **always** `204`, whether or not the address exists.

### `POST /api/v1/auth/password-reset/confirm`
`{ "token":"…", "password":"…" }` → `204`. Invalidates every session of that user.

## Session required

### `GET /api/v1/me`
The single bootstrap call the app makes on load.
```json
{ "user": User, "prefs": Prefs, "calendars": [CalendarView], "family_tz": "Europe/Paris",
  "app_version": "a1b2c3d4", "server_time": "2026-07-26T09:00:00Z" }
```

### `PATCH /api/v1/me`
Any subset of `display_name`, `color`, `lang`, `week_start` (0=Sunday…6=Saturday),
`time_format` (`"24h"|"12h"`). Password change: `{"current_password":"…","new_password":"…"}`
(invalidates all other sessions). → `{ "user": User }`

### `PUT /api/v1/me/avatar`
Raw bytes, `Content-Type: image/jpeg` or `image/png`, ≤ 1 MB. The client resizes to ≤ 256 px
before upload; the server re-encodes to a 128 px JPEG and stores only that. → `{ "has_avatar": true }`
`DELETE /api/v1/me/avatar` → `204`.

### `GET /api/v1/users/{id}/avatar`
`image/jpeg` with `X-Content-Type-Options: nosniff`, or `404`. Cacheable.

### `POST /api/v1/auth/logout`
→ `204`. The client first unsubscribes push and calls `DELETE /api/v1/push/subscription`.

## Calendars

`CalendarView` is what `/me` returns per calendar:
```json
{ "id":1, "name":"Family", "color":"#3b7ddd", "creator_id":1,
  "membership": { "muted":false, "participating_only":false },
  "members": [ { "user_id":1, "display_name":"Mum", "color":"#c0392b", "has_avatar":true } ],
  "labels": [ { "id":1, "name":"Emerald green", "color":"#2ecc87", "position":0 } ] }
```

| Method | Path | Body / notes |
|---|---|---|
| `POST` | `/api/v1/calendars` | `{name,color}` → `CalendarView` (creator joins, 10 labels seeded) |
| `PATCH` | `/api/v1/calendars/{id}` | `{name?,color?}` |
| `DELETE` | `/api/v1/calendars/{id}` | only when the caller is the sole member → `204`; otherwise `409` |
| `POST` | `/api/v1/calendars/{id}/leave` | `204`; creator role passes to the longest-standing member |
| `PATCH` | `/api/v1/calendars/{id}/membership` | `{muted?,participating_only?}` (the caller's own) |
| `DELETE` | `/api/v1/calendars/{id}/members/{user_id}` | creator only → `204` |
| `PATCH` | `/api/v1/calendars/{id}/labels/{label_id}` | `{name?,color?,position?}` — labels are never created or deleted |
| `POST` | `/api/v1/calendars/{id}/invites` | → `{ "id":…, "token":"…", "url":"https://…/join/…", "expires_at":"…" }` (token shown once) |
| `GET` | `/api/v1/calendars/{id}/invites` | active invites (no tokens) |
| `POST` | `/api/v1/invites/{id}/revoke` | `204` |
| `PUT` | `/api/v1/calendars/{id}/image` | raw JPEG/PNG bytes, ≤ 1 MB, like the avatar route; server re-encodes to a 128 px JPEG → `{ "has_image": true }` |
| `DELETE` | `/api/v1/calendars/{id}/image` | `204` |
| `GET` | `/api/v1/calendars/{id}/image` | `image/jpeg` with `nosniff` and an ETag, or `404` when the calendar has no picture |

`CalendarView` carries `has_image`, so the client knows whether to fetch the picture or
fall back to a tinted calendar glyph. Any member may set the picture: permissions are
flat here as everywhere else.

## Events

### `GET /api/v1/events?from=&to=&calendar_ids=`
The main read. `from`/`to` are inclusive family-tz dates; `calendar_ids` is optional CSV
(default: all the caller's calendars). Occurrences of recurring series are expanded server-side.
```json
{ "occurrences": [ Occurrence ],
  "holidays": [ { "date":"2026-08-15", "name":"Assomption" } ] }
```

`Occurrence`:
```json
{ "event_id": 12, "calendar_id": 1, "calendar_color": "#3b7ddd",
  "title": "Leo's dentist", "all_day": false,
  "starts_at": "2026-08-04T14:30:00Z", "ends_at": "2026-08-04T15:15:00Z",
  "start_date": null, "end_date": null,
  "occurrence_date": "2026-08-04",
  "location": "", "url": "", "notes": "",
  "label_id": 3, "label_color": "#47b2f7", "label_name": "Sky blue",
  "participants": [2,3],
  "recurrence_id": 5, "series_event_id": 12, "is_override": false,
  "created_by": 2, "updated_at": "2026-07-01T10:00:00Z" }
```
`occurrence_date` identifies the instance within its series and is required by every
scoped edit. For non-recurring events it is the start date.

An edited occurrence is stored as a standalone copy of the event, and `event_id` is that
copy's id — `series_event_id` is the series it belongs to. Sending the copy's id back to
any of the routes below addresses **that occurrence**: the server resolves it to its
series and to the `occurrence_date` it stands for, whatever `date` the request carries,
and `scope` defaults to `this` when none is given. So a client may hold the id it was
given and keep editing the same occurrence, and `scope=all` from there still reaches the
whole series.

### `POST /api/v1/events`
```json
{ "calendar_id":1, "title":"Swimming", "all_day":false,
  "starts_at":"2026-08-04T14:30:00Z", "ends_at":"2026-08-04T15:15:00Z",
  "location":"", "url":"", "notes":"", "label_id":3, "participants":[2,3],
  "recurrence": { "freq":"weekly", "interval":1, "by_weekday":[2], "until":"2026-12-31" },
  "reminders": [ { "offset_minutes": 1440 } ] }
```
All-day form omits `starts_at`/`ends_at` and sends `start_date`/`end_date` (inclusive), with
reminders as `{ "days_before":1, "at_time_local":"09:00" }`. `recurrence` and `reminders` are
optional; `reminders` apply to the caller only. → `201 { "event": Event }`

### `GET /api/v1/events/{id}?date=YYYY-MM-DD`
Detail for one occurrence (`date` = `occurrence_date`, required for series, ignored for
an edited occurrence's own id). `recurrence` is the **series'**, so an edited occurrence
reports the pattern it belongs to rather than `null`, and a client can go on offering the
scope question.
→ `{ "occurrence": Occurrence, "my_reminders": [Reminder], "recurrence": Recurrence|null }`

### `PATCH /api/v1/events/{id}?scope=this|upcoming|all&date=YYYY-MM-DD`
Same body as POST minus `calendar_id`. `scope` and `date` are required when the event
belongs to a series; `scope=this` creates an override, `upcoming` splits the series,
`all` edits the template (existing overrides are deliberate edits and are left alone).
→ `{ "event": Event }`

### `DELETE /api/v1/events/{id}?scope=&date=`
`this` cancels one occurrence, `upcoming` ends the series before it, `all` deletes
series + overrides + reminders + queued notifications. → `204`

### `PUT /api/v1/events/{id}/reminders`
Replaces **the caller's** reminders for the event or its series.
`{ "reminders": [ { "offset_minutes": 30 } ] }` → `{ "reminders": [Reminder] }`

### `GET /api/v1/search?q=&participant=&label_id=&calendar_id=`
Case- and accent-insensitive (`ecole` matches `École`). A recurring series appears once.
→ `{ "results": [ { "event": Event, "next_occurrence": "2026-09-01" } ] }`

## Notifications

| Method | Path | Body / notes |
|---|---|---|
| `GET` | `/api/v1/prefs` | → `Prefs` |
| `PATCH` | `/api/v1/prefs` | any subset of `Prefs` |
| `POST` | `/api/v1/push/subscription` | `{endpoint,p256dh,auth,ua_label}` — idempotent per endpoint |
| `POST` | `/api/v1/push/confirm` | `{endpoint}` — liveness ping on every app open; bumps `last_confirmed_at` |
| `DELETE` | `/api/v1/push/subscription` | `{endpoint}` — on logout |
| `POST` | `/api/v1/push/test` | sends a test notification to the caller's devices → `{"sent":2}` |
| `GET` | `/api/v1/activity?limit=50&before=` | → `{ "activity": [Activity] }` |

`Prefs`:
```json
{ "digest_enabled": true, "digest_time": "07:30", "digest_on_empty": false,
  "daily_summary_mode": false, "summary_time": "20:00",
  "email_reminders": true, "email_digest": false, "activity_push": true,
  "push_health": { "devices": 2, "stale": false, "last_confirmed_at": "2026-07-26T08:00:00Z" } }
```
`push_health.stale` is true when no device has confirmed within 14 days: the client shows the
repair banner and email is forced on.

## Push payload (server → service worker)

```json
{ "kind":"reminder", "title":"Leo's dentist", "body":"Tomorrow at 14:30",
  "url":"/#/event/12/2026-08-04", "tag":"reminder:12:2026-08-04", "lang":"en" }
```
Kept under ~3 900 bytes (aes128gcm ceiling is 4 096). Digests carry a count and the first few
truncated titles; the full day is fetched when the notification is clicked.

### `GET /locales/{lang}.json`
The translation catalog the browser loads, served from the same files the server uses
for notification text. `fr` and `en`; anything else is a 404.

## Operational

- `GET /healthz` — no auth. `{"status":"ok|degraded", "checks":{…}}`, 200 or 503.
- Dev mode only (`ALMANACK_DEV=1`), never mounted in production:
  `GET /dev/` (dashboard), `GET /dev/mail`, `GET /dev/notifications`,
  `POST /dev/clock {"advance":"26h"}` or `{"set":"2026-08-04T06:00:00Z"}`,
  `POST /dev/tick` (run planner + scheduler immediately), `POST /dev/seed`.
