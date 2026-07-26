-- Initial schema.
--
-- Storage conventions, chosen so that a database file opened in 2040 explains itself:
--   * instants  -> TEXT, RFC 3339 UTC with a trailing Z ("2026-08-04T14:30:00Z")
--   * dates     -> TEXT, "YYYY-MM-DD", with no timezone and no time-of-day
--   * booleans  -> INTEGER 0/1
-- All-day events carry dates only. Timed events carry instants only. The CHECK
-- constraints below make the wrong combination unrepresentable rather than merely
-- discouraged.

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    email         TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT    NOT NULL,
    display_name  TEXT    NOT NULL,
    color         TEXT    NOT NULL,
    lang          TEXT    NOT NULL DEFAULT 'fr'  CHECK (lang IN ('fr','en')),
    week_start    INTEGER NOT NULL DEFAULT 1     CHECK (week_start BETWEEN 0 AND 6),
    time_format   TEXT    NOT NULL DEFAULT '24h' CHECK (time_format IN ('24h','12h')),
    avatar        BLOB,
    is_admin      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT    NOT NULL
);

CREATE TABLE sessions (
    id           INTEGER PRIMARY KEY,
    token_hash   TEXT    NOT NULL UNIQUE,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TEXT    NOT NULL,
    last_seen_at TEXT    NOT NULL,
    expires_at   TEXT    NOT NULL
);
CREATE INDEX idx_sessions_user    ON sessions(user_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

CREATE TABLE password_resets (
    id         INTEGER PRIMARY KEY,
    token_hash TEXT    NOT NULL UNIQUE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT    NOT NULL,
    expires_at TEXT    NOT NULL,
    used_at    TEXT
);
CREATE INDEX idx_resets_user ON password_resets(user_id);

CREATE TABLE calendars (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    color      TEXT    NOT NULL,
    creator_id INTEGER NOT NULL REFERENCES users(id),
    created_at TEXT    NOT NULL
);

-- Per-calendar notification settings live here, not in notification_prefs: muting is a
-- property of the (user, calendar) pair.
CREATE TABLE calendar_members (
    calendar_id        INTEGER NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    user_id            INTEGER NOT NULL REFERENCES users(id)     ON DELETE CASCADE,
    muted              INTEGER NOT NULL DEFAULT 0,
    participating_only INTEGER NOT NULL DEFAULT 0,
    joined_at          TEXT    NOT NULL,
    PRIMARY KEY (calendar_id, user_id)
);
CREATE INDEX idx_members_user ON calendar_members(user_id);

-- Exactly ten rows are seeded per calendar and are then renamed/recoloured/reordered
-- forever. Nothing creates or deletes labels, which is what lets events.label_id stay
-- NOT NULL.
CREATE TABLE labels (
    id          INTEGER PRIMARY KEY,
    calendar_id INTEGER NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    name        TEXT    NOT NULL,
    color       TEXT    NOT NULL,
    position    INTEGER NOT NULL
);
CREATE INDEX idx_labels_calendar ON labels(calendar_id, position);

CREATE TABLE invites (
    id          INTEGER PRIMARY KEY,
    token_hash  TEXT    NOT NULL UNIQUE,
    calendar_id INTEGER NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    created_by  INTEGER NOT NULL REFERENCES users(id),
    created_at  TEXT    NOT NULL,
    expires_at  TEXT    NOT NULL,
    revoked_at  TEXT,
    used_count  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_invites_calendar ON invites(calendar_id);

-- A recurrence describes which dates repeat; the time of day and duration stay on the
-- event. Structured columns rather than an RRULE string: queryable and checkable.
CREATE TABLE recurrences (
    id             INTEGER PRIMARY KEY,
    freq           TEXT    NOT NULL CHECK (freq IN ('daily','weekly','monthly','yearly')),
    interval       INTEGER NOT NULL DEFAULT 1 CHECK (interval >= 1),
    by_weekday     TEXT,           -- CSV of 0..6, Sunday=0 (weekly set, or the day for week_ordinal)
    by_monthday    INTEGER CHECK (by_monthday IS NULL OR by_monthday BETWEEN 1 AND 31),
    week_ordinal   INTEGER CHECK (week_ordinal IS NULL OR week_ordinal IN (1,2,3,4,5,-1)),
    month_last_day INTEGER NOT NULL DEFAULT 0,
    until          TEXT,           -- inclusive; NULL means forever
    dtstart        TEXT    NOT NULL,
    -- "2nd Tuesday" needs a weekday to go with the ordinal
    CHECK (week_ordinal IS NULL OR by_weekday IS NOT NULL),
    -- a monthly rule picks exactly one of the three ways to say which day
    CHECK (freq <> 'monthly' OR
           ((by_monthday IS NOT NULL) + (week_ordinal IS NOT NULL) + (month_last_day <> 0)) = 1)
);

CREATE TABLE events (
    id            INTEGER PRIMARY KEY,
    calendar_id   INTEGER NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    title         TEXT    NOT NULL,
    -- lowercased, diacritics stripped, title+location+notes: makes LIKE search find
    -- "ecole" in "École". Maintained by the store on every write.
    search_norm   TEXT    NOT NULL DEFAULT '',
    all_day       INTEGER NOT NULL DEFAULT 0,
    starts_at     TEXT,
    ends_at       TEXT,
    start_date    TEXT,
    end_date      TEXT,    -- inclusive
    location      TEXT    NOT NULL DEFAULT '',
    url           TEXT    NOT NULL DEFAULT '',
    notes         TEXT    NOT NULL DEFAULT '',
    label_id      INTEGER NOT NULL REFERENCES labels(id),
    recurrence_id INTEGER REFERENCES recurrences(id) ON DELETE SET NULL,
    created_by    INTEGER NOT NULL REFERENCES users(id),
    created_at    TEXT    NOT NULL,
    updated_by    INTEGER NOT NULL REFERENCES users(id),
    updated_at    TEXT    NOT NULL,
    CHECK ((all_day = 1 AND starts_at IS NULL     AND ends_at IS NULL
                        AND start_date IS NOT NULL AND end_date IS NOT NULL)
        OR (all_day = 0 AND starts_at IS NOT NULL AND ends_at IS NOT NULL
                        AND start_date IS NULL     AND end_date IS NULL))
);
CREATE INDEX idx_events_cal_starts ON events(calendar_id, starts_at);
CREATE INDEX idx_events_cal_dates  ON events(calendar_id, start_date);
CREATE INDEX idx_events_recurrence ON events(recurrence_id);
CREATE INDEX idx_events_search     ON events(calendar_id, search_norm);

-- One row per edited or cancelled occurrence of a series.
-- override_event_id NULL means the occurrence is cancelled; otherwise it points at a
-- standalone event carrying the edited values. The series itself is never mutated by a
-- single-occurrence edit.
CREATE TABLE event_overrides (
    recurrence_id     INTEGER NOT NULL REFERENCES recurrences(id) ON DELETE CASCADE,
    occurrence_date   TEXT    NOT NULL,  -- family-tz date of the ORIGINAL occurrence
    override_event_id INTEGER REFERENCES events(id) ON DELETE CASCADE,
    PRIMARY KEY (recurrence_id, occurrence_date)
);
CREATE INDEX idx_overrides_event ON event_overrides(override_event_id);

CREATE TABLE event_participants (
    event_id INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    user_id  INTEGER NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    PRIMARY KEY (event_id, user_id)
);
CREATE INDEX idx_participants_user ON event_participants(user_id);

-- Reminders are per user: creating an event never sets reminders for anyone else.
-- Timed events use offset_minutes; all-day events use days_before + at_time_local,
-- because "09:00 on the day" cannot be expressed as an offset before midnight.
CREATE TABLE reminders (
    id             INTEGER PRIMARY KEY,
    event_id       INTEGER REFERENCES events(id)      ON DELETE CASCADE,
    recurrence_id  INTEGER REFERENCES recurrences(id) ON DELETE CASCADE,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    offset_minutes INTEGER,
    days_before    INTEGER,
    at_time_local  TEXT,
    CHECK ((event_id IS NULL) <> (recurrence_id IS NULL)),
    CHECK ((offset_minutes IS NOT NULL AND days_before IS NULL AND at_time_local IS NULL)
        OR (offset_minutes IS NULL     AND days_before IS NOT NULL AND at_time_local IS NOT NULL))
);
CREATE INDEX idx_reminders_event      ON reminders(event_id);
CREATE INDEX idx_reminders_recurrence ON reminders(recurrence_id);
CREATE INDEX idx_reminders_user       ON reminders(user_id);

-- One row per browser profile per device. last_confirmed_at is bumped by the client's
-- liveness check: a dead iOS subscription keeps returning success to the server, so
-- delivery errors alone never reveal it.
CREATE TABLE push_subscriptions (
    id                INTEGER PRIMARY KEY,
    user_id           INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    endpoint          TEXT    NOT NULL UNIQUE,
    p256dh            TEXT    NOT NULL,
    auth              TEXT    NOT NULL,
    ua_label          TEXT    NOT NULL DEFAULT '',
    created_at        TEXT    NOT NULL,
    last_ok_at        TEXT,
    last_confirmed_at TEXT,
    failures          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_push_user ON push_subscriptions(user_id);

CREATE TABLE notification_prefs (
    user_id            INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    digest_enabled     INTEGER NOT NULL DEFAULT 1,
    digest_time        TEXT    NOT NULL DEFAULT '07:30',
    digest_on_empty    INTEGER NOT NULL DEFAULT 0,
    daily_summary_mode INTEGER NOT NULL DEFAULT 0,
    summary_time       TEXT    NOT NULL DEFAULT '20:00',
    -- email defaults ON: iOS push dies silently, so email is a parallel channel and
    -- not something switched on after a failure that is never observed
    email_reminders    INTEGER NOT NULL DEFAULT 1,
    email_digest       INTEGER NOT NULL DEFAULT 0,
    activity_push      INTEGER NOT NULL DEFAULT 1
);

-- The durable outbox. UNIQUE makes planner idempotency structural rather than a
-- property someone has to preserve by being careful.
CREATE TABLE notification_queue (
    id                 INTEGER PRIMARY KEY,
    user_id            INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind               TEXT    NOT NULL CHECK (kind IN ('reminder','digest','activity','summary')),
    source_ref         TEXT    NOT NULL,   -- e.g. "reminder:12:2026-08-04"
    payload            TEXT    NOT NULL,   -- JSON
    due_at             TEXT    NOT NULL,
    sending_started_at TEXT,
    sent_at            TEXT,
    skipped            TEXT,               -- reason, when delivered stale or dropped
    attempts           INTEGER NOT NULL DEFAULT 0,
    UNIQUE (user_id, kind, source_ref, due_at)
);
CREATE INDEX idx_queue_due ON notification_queue(sent_at, due_at);

CREATE TABLE activity_log (
    id          INTEGER PRIMARY KEY,
    calendar_id INTEGER NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    action      TEXT    NOT NULL,
    event_id    INTEGER,          -- deliberately not a FK: the row outlives the event
    title       TEXT    NOT NULL DEFAULT '',
    at          TEXT    NOT NULL
);
CREATE INDEX idx_activity_calendar ON activity_log(calendar_id, at);

-- French public holidays are computed, not stored. These rows let the family add a
-- date or suppress one when the law changes, without a rebuild.
CREATE TABLE holiday_overrides (
    date TEXT PRIMARY KEY,
    name TEXT              -- NULL suppresses a computed holiday
);

-- Small key/value store for operational state that has no natural home: the planner's
-- materialization horizon, the last backup result, the scheduler heartbeat.
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
