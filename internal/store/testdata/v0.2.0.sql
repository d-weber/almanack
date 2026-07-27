-- A family's Almanack database as the v0.2.0 release left it: schema version 0002,
-- four people, three calendars, a fortnight of events and an outbox in mid-flight.
--
-- TestUpgradeFromReleasedDatabase loads this into an empty file, opens it with the
-- current binary, and checks that every row is still there and still readable through
-- the store API after the pending migrations have run. It is the evidence for the
-- promise in CONVENTIONS.md §8 that an existing calendar survives an upgrade.
--
-- Written by the v0.2.0 binary itself, against a fake clock fixed at
-- 2026-07-27T09:00:00Z, with literal dates, argon2id salts and token hashes: nothing
-- in it is drawn from the machine or the moment it was generated. It is SQL text
-- rather than a .db file so that it is reviewable in a diff and still readable in ten
-- years by someone holding nothing but a text editor.
--
-- DO NOT REGENERATE IT AGAINST A NEWER SCHEMA. Its value is that it is old; refreshed
-- to head it would prove only that head opens head. The test pins the version it
-- expects and fails if this file moves. A later release wanting its own fixture should
-- add a second file beside this one and a second row in the test's table.

CREATE TABLE schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		);
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
, image BLOB);
CREATE TABLE calendar_members (
    calendar_id        INTEGER NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    user_id            INTEGER NOT NULL REFERENCES users(id)     ON DELETE CASCADE,
    muted              INTEGER NOT NULL DEFAULT 0,
    participating_only INTEGER NOT NULL DEFAULT 0,
    joined_at          TEXT    NOT NULL,
    PRIMARY KEY (calendar_id, user_id)
);
CREATE INDEX idx_members_user ON calendar_members(user_id);
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
CREATE TABLE holiday_overrides (
    date TEXT PRIMARY KEY,
    name TEXT              -- NULL suppresses a computed holiday
);
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- schema_migrations (2 rows)
INSERT INTO "schema_migrations" ("version", "applied_at") VALUES (1, '2026-07-27T09:00:00Z');
INSERT INTO "schema_migrations" ("version", "applied_at") VALUES (2, '2026-07-27T09:00:00Z');

-- users (4 rows)
INSERT INTO "users" ("id", "email", "password_hash", "display_name", "color", "lang", "week_start", "time_format", "avatar", "is_admin", "created_at") VALUES (1, 'mum@example.org', '$argon2id$v=19$m=65536,t=3,p=4$YWxtYW5hY2stZml4dC0wMQ$uJqv7kqUovSb7656STFbS5UG5mc1Cs9WkroeDdazack', 'Mum', '#c0392b', 'en', 1, '24h', NULL, 1, '2026-07-27T09:00:00Z');
INSERT INTO "users" ("id", "email", "password_hash", "display_name", "color", "lang", "week_start", "time_format", "avatar", "is_admin", "created_at") VALUES (2, 'dad@example.org', '$argon2id$v=19$m=65536,t=3,p=4$YWxtYW5hY2stZml4dC0wMg$4Dp0/D+Aho6VWcI2/zrk+F041igfXRCk100RKK9ES1c', 'Dad', '#2980b9', 'en', 1, '24h', NULL, 0, '2026-07-27T09:01:00Z');
INSERT INTO "users" ("id", "email", "password_hash", "display_name", "color", "lang", "week_start", "time_format", "avatar", "is_admin", "created_at") VALUES (3, 'leo@example.org', '$argon2id$v=19$m=65536,t=3,p=4$YWxtYW5hY2stZml4dC0wMw$uPhUwbe8g+g45AP9wKHR/8QfegPGnt6qm2yv61ImK5k', 'Leo', '#27ae60', 'en', 1, '12h', X'89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4890000000a49444154789c63000100000500010d0a2db40000000049454e44ae426082', 0, '2026-07-27T09:02:00Z');
INSERT INTO "users" ("id", "email", "password_hash", "display_name", "color", "lang", "week_start", "time_format", "avatar", "is_admin", "created_at") VALUES (4, 'gran@example.org', '$argon2id$v=19$m=65536,t=3,p=4$YWxtYW5hY2stZml4dC0wNA$cnlGZALv7LzXDi6bCOW4tY2x/iZlWp4pFe3OoVDUe0g', 'Gran', '#8e44ad', 'fr', 0, '24h', NULL, 0, '2026-07-27T09:03:00Z');

-- sessions (2 rows)
INSERT INTO "sessions" ("id", "token_hash", "user_id", "created_at", "last_seen_at", "expires_at") VALUES (1, '0f9a2c1e5b7d4a3f8e6c0b5d2a91746c3e8f0a1b2c3d4e5f60718293a4b5c6d7', 1, '2026-07-27T09:20:00Z', '2026-07-27T09:20:00Z', '2026-10-25T09:00:00Z');
INSERT INTO "sessions" ("id", "token_hash", "user_id", "created_at", "last_seen_at", "expires_at") VALUES (2, '1a8b3d2f6c5e4b0a9f7d1c6e3b0a8574d2f9e1b0c3d5a7f92b4c6d8e0f1a2b34', 2, '2026-07-27T09:20:00Z', '2026-07-27T09:20:00Z', '2026-10-25T09:00:00Z');

-- password_resets (1 row)
INSERT INTO "password_resets" ("id", "token_hash", "user_id", "created_at", "expires_at", "used_at") VALUES (1, '2b7c4e1d0a9f8e3b5c6d7a2f1e0b9c8d3a4f5e6b7c8d9e0a1b2c3d4e5f607182', 4, '2026-07-27T09:20:00Z', '2026-07-27T09:30:00Z', NULL);

-- calendars (3 rows)
INSERT INTO "calendars" ("id", "name", "color", "creator_id", "created_at", "image") VALUES (1, 'Family', '#3b7ddd', 1, '2026-07-27T09:04:00Z', X'89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4890000000a49444154789c63000100000500010d0a2db40000000049454e44ae426082');
INSERT INTO "calendars" ("id", "name", "color", "creator_id", "created_at", "image") VALUES (2, 'Parents', '#7d3bdd', 1, '2026-07-27T09:06:30Z', NULL);
INSERT INTO "calendars" ("id", "name", "color", "creator_id", "created_at", "image") VALUES (3, 'Kids'' activities', '#2fa84f', 2, '2026-07-27T09:08:00Z', NULL);

-- calendar_members (9 rows)
INSERT INTO "calendar_members" ("calendar_id", "user_id", "muted", "participating_only", "joined_at") VALUES (1, 1, 0, 0, '2026-07-27T09:04:00Z');
INSERT INTO "calendar_members" ("calendar_id", "user_id", "muted", "participating_only", "joined_at") VALUES (1, 2, 0, 0, '2026-07-27T09:05:00Z');
INSERT INTO "calendar_members" ("calendar_id", "user_id", "muted", "participating_only", "joined_at") VALUES (1, 3, 0, 0, '2026-07-27T09:05:30Z');
INSERT INTO "calendar_members" ("calendar_id", "user_id", "muted", "participating_only", "joined_at") VALUES (1, 4, 1, 1, '2026-07-27T09:06:00Z');
INSERT INTO "calendar_members" ("calendar_id", "user_id", "muted", "participating_only", "joined_at") VALUES (2, 1, 0, 0, '2026-07-27T09:06:30Z');
INSERT INTO "calendar_members" ("calendar_id", "user_id", "muted", "participating_only", "joined_at") VALUES (2, 2, 0, 0, '2026-07-27T09:07:30Z');
INSERT INTO "calendar_members" ("calendar_id", "user_id", "muted", "participating_only", "joined_at") VALUES (3, 2, 0, 0, '2026-07-27T09:08:00Z');
INSERT INTO "calendar_members" ("calendar_id", "user_id", "muted", "participating_only", "joined_at") VALUES (3, 1, 0, 0, '2026-07-27T09:09:00Z');
INSERT INTO "calendar_members" ("calendar_id", "user_id", "muted", "participating_only", "joined_at") VALUES (3, 3, 0, 0, '2026-07-27T09:09:30Z');

-- labels (30 rows)
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (1, 1, 'Emerald green', '#2ecc87', 0);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (2, 1, 'Lagoon teal', '#3dc2c8', 1);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (3, 1, 'Holidays', '#e67e22', 2);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (4, 1, 'Warm taupe', '#948078', 3);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (5, 1, 'Midnight black', '#212121', 4);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (6, 1, 'Poppy red', '#e73b3b', 5);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (7, 1, 'Raspberry pink', '#f35f8c', 6);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (8, 1, 'Sunset coral', '#fb7f77', 7);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (9, 1, 'Golden amber', '#fdc02d', 8);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (10, 1, 'Soft lilac', '#b38bdc', 9);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (11, 2, 'Emerald green', '#2ecc87', 0);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (12, 2, 'Lagoon teal', '#3dc2c8', 1);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (13, 2, 'Sky blue', '#47b2f7', 2);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (14, 2, 'Warm taupe', '#948078', 3);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (15, 2, 'Midnight black', '#212121', 4);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (16, 2, 'Poppy red', '#e73b3b', 5);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (17, 2, 'Raspberry pink', '#f35f8c', 6);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (18, 2, 'Sunset coral', '#fb7f77', 7);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (19, 2, 'Golden amber', '#fdc02d', 8);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (20, 2, 'Soft lilac', '#b38bdc', 9);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (21, 3, 'Emerald green', '#2ecc87', 0);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (22, 3, 'Lagoon teal', '#3dc2c8', 1);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (23, 3, 'Sky blue', '#47b2f7', 2);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (24, 3, 'Warm taupe', '#948078', 3);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (25, 3, 'Midnight black', '#212121', 4);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (26, 3, 'Poppy red', '#e73b3b', 5);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (27, 3, 'Raspberry pink', '#f35f8c', 6);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (28, 3, 'Sunset coral', '#fb7f77', 7);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (29, 3, 'Golden amber', '#fdc02d', 8);
INSERT INTO "labels" ("id", "calendar_id", "name", "color", "position") VALUES (30, 3, 'Soft lilac', '#b38bdc', 9);

-- invites (2 rows)
INSERT INTO "invites" ("id", "token_hash", "calendar_id", "created_by", "created_at", "expires_at", "revoked_at", "used_count") VALUES (1, '3c6d5f2e1b0a9d8c7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d', 1, 1, '2026-07-27T09:20:00Z', '2026-08-03T09:00:00Z', NULL, 0);
INSERT INTO "invites" ("id", "token_hash", "calendar_id", "created_by", "created_at", "expires_at", "revoked_at", "used_count") VALUES (2, '4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c', 2, 1, '2026-07-27T09:20:00Z', '2026-08-03T09:00:00Z', '2026-07-27T10:00:00Z', 0);

-- recurrences (4 rows)
INSERT INTO "recurrences" ("id", "freq", "interval", "by_weekday", "by_monthday", "week_ordinal", "month_last_day", "until", "dtstart") VALUES (1, 'weekly', 1, '2', NULL, NULL, 0, NULL, '2026-07-28');
INSERT INTO "recurrences" ("id", "freq", "interval", "by_weekday", "by_monthday", "week_ordinal", "month_last_day", "until", "dtstart") VALUES (2, 'yearly', 1, NULL, NULL, NULL, 0, NULL, '2026-03-12');
INSERT INTO "recurrences" ("id", "freq", "interval", "by_weekday", "by_monthday", "week_ordinal", "month_last_day", "until", "dtstart") VALUES (3, 'monthly', 1, NULL, NULL, NULL, 1, NULL, '2026-07-31');
INSERT INTO "recurrences" ("id", "freq", "interval", "by_weekday", "by_monthday", "week_ordinal", "month_last_day", "until", "dtstart") VALUES (4, 'weekly', 2, '3', NULL, NULL, 0, NULL, '2026-07-29');

-- events (9 rows)
INSERT INTO "events" ("id", "calendar_id", "title", "search_norm", "all_day", "starts_at", "ends_at", "start_date", "end_date", "location", "url", "notes", "label_id", "recurrence_id", "created_by", "created_at", "updated_by", "updated_at") VALUES (1, 1, 'Leo''s dentist', 'leo''s dentist bridge street dental bring the referral letter', 0, '2026-07-27T14:30:00Z', '2026-07-27T15:15:00Z', NULL, NULL, 'Bridge Street Dental', '', 'Bring the referral letter', 1, NULL, 1, '2026-07-27T09:10:00Z', 1, '2026-07-27T09:10:00Z');
INSERT INTO "events" ("id", "calendar_id", "title", "search_norm", "all_day", "starts_at", "ends_at", "start_date", "end_date", "location", "url", "notes", "label_id", "recurrence_id", "created_by", "created_at", "updated_by", "updated_at") VALUES (2, 1, 'Parents'' evening', 'parents'' evening elm park school ', 0, '2026-07-28T16:00:00Z', '2026-07-28T17:00:00Z', NULL, NULL, 'Elm Park School', '', '', 2, NULL, 2, '2026-07-27T09:11:00Z', 2, '2026-07-27T09:11:00Z');
INSERT INTO "events" ("id", "calendar_id", "title", "search_norm", "all_day", "starts_at", "ends_at", "start_date", "end_date", "location", "url", "notes", "label_id", "recurrence_id", "created_by", "created_at", "updated_by", "updated_at") VALUES (3, 1, 'Seaside holiday', 'seaside holiday whitstable ', 1, NULL, NULL, '2026-08-06', '2026-08-12', 'Whitstable', 'https://example.org/cottage', '', 3, NULL, 1, '2026-07-27T09:12:00Z', 1, '2026-07-27T09:12:00Z');
INSERT INTO "events" ("id", "calendar_id", "title", "search_norm", "all_day", "starts_at", "ends_at", "start_date", "end_date", "location", "url", "notes", "label_id", "recurrence_id", "created_by", "created_at", "updated_by", "updated_at") VALUES (4, 3, 'Swimming', 'swimming leisure centre ', 0, '2026-07-28T15:30:00Z', '2026-07-28T16:30:00Z', NULL, NULL, 'Leisure centre', '', '', 24, 1, 2, '2026-07-27T09:13:00Z', 2, '2026-07-27T09:13:00Z');
INSERT INTO "events" ("id", "calendar_id", "title", "search_norm", "all_day", "starts_at", "ends_at", "start_date", "end_date", "location", "url", "notes", "label_id", "recurrence_id", "created_by", "created_at", "updated_by", "updated_at") VALUES (5, 3, 'Swimming (later than usual)', 'swimming (later than usual) leisure centre ', 0, '2026-08-04T17:00:00Z', '2026-08-04T18:00:00Z', NULL, NULL, 'Leisure centre', '', '', 24, NULL, 2, '2026-07-27T09:14:00Z', 2, '2026-07-27T09:14:00Z');
INSERT INTO "events" ("id", "calendar_id", "title", "search_norm", "all_day", "starts_at", "ends_at", "start_date", "end_date", "location", "url", "notes", "label_id", "recurrence_id", "created_by", "created_at", "updated_by", "updated_at") VALUES (6, 1, 'Leo''s birthday', 'leo''s birthday  ', 1, NULL, NULL, '2026-03-12', '2026-03-12', '', '', '', 5, 2, 1, '2026-07-27T09:16:00Z', 1, '2026-07-27T09:16:00Z');
INSERT INTO "events" ("id", "calendar_id", "title", "search_norm", "all_day", "starts_at", "ends_at", "start_date", "end_date", "location", "url", "notes", "label_id", "recurrence_id", "created_by", "created_at", "updated_by", "updated_at") VALUES (7, 2, 'Household accounts', 'household accounts  ', 1, NULL, NULL, '2026-07-31', '2026-07-31', '', '', '', 16, 3, 1, '2026-07-27T09:17:00Z', 1, '2026-07-27T09:17:00Z');
INSERT INTO "events" ("id", "calendar_id", "title", "search_norm", "all_day", "starts_at", "ends_at", "start_date", "end_date", "location", "url", "notes", "label_id", "recurrence_id", "created_by", "created_at", "updated_by", "updated_at") VALUES (8, 2, 'Cinema', 'cinema  ', 0, '2026-08-01T18:30:00Z', '2026-08-01T20:45:00Z', NULL, NULL, '', '', '', 17, NULL, 2, '2026-07-27T09:18:00Z', 2, '2026-07-27T09:18:00Z');
INSERT INTO "events" ("id", "calendar_id", "title", "search_norm", "all_day", "starts_at", "ends_at", "start_date", "end_date", "location", "url", "notes", "label_id", "recurrence_id", "created_by", "created_at", "updated_by", "updated_at") VALUES (9, 3, 'Guitar lesson', 'guitar lesson  ', 0, '2026-07-29T12:00:00Z', '2026-07-29T13:00:00Z', NULL, NULL, '', '', '', 28, 4, 1, '2026-07-27T09:19:00Z', 1, '2026-07-27T09:19:00Z');

-- event_overrides (2 rows)
INSERT INTO "event_overrides" ("recurrence_id", "occurrence_date", "override_event_id") VALUES (1, '2026-08-04', 5);
INSERT INTO "event_overrides" ("recurrence_id", "occurrence_date", "override_event_id") VALUES (1, '2026-08-18', NULL);

-- event_participants (18 rows)
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (1, 1);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (1, 3);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (2, 1);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (2, 2);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (3, 1);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (3, 2);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (3, 3);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (3, 4);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (4, 2);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (4, 3);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (5, 2);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (5, 3);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (6, 3);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (7, 1);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (7, 2);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (8, 1);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (8, 2);
INSERT INTO "event_participants" ("event_id", "user_id") VALUES (9, 3);

-- reminders (3 rows)
INSERT INTO "reminders" ("id", "event_id", "recurrence_id", "user_id", "offset_minutes", "days_before", "at_time_local") VALUES (1, 1, NULL, 1, 1440, NULL, NULL);
INSERT INTO "reminders" ("id", "event_id", "recurrence_id", "user_id", "offset_minutes", "days_before", "at_time_local") VALUES (2, 3, NULL, 2, NULL, 2, '09:00');
INSERT INTO "reminders" ("id", "event_id", "recurrence_id", "user_id", "offset_minutes", "days_before", "at_time_local") VALUES (3, NULL, 1, 2, 30, NULL, NULL);

-- push_subscriptions (3 rows)
INSERT INTO "push_subscriptions" ("id", "user_id", "endpoint", "p256dh", "auth", "ua_label", "created_at", "last_ok_at", "last_confirmed_at", "failures") VALUES (1, 1, 'https://push.example.org/f/mum-phone', 'BJ4Q1sJmMFJ0', 'Xr9v2sKq', 'iPhone', '2026-07-27T09:20:00Z', '2026-07-27T07:00:00Z', '2026-07-27T07:30:00Z', 0);
INSERT INTO "push_subscriptions" ("id", "user_id", "endpoint", "p256dh", "auth", "ua_label", "created_at", "last_ok_at", "last_confirmed_at", "failures") VALUES (2, 1, 'https://push.example.org/f/mum-laptop', 'BK7T2xQnNGL1', 'Yt8w3tLr', 'Firefox on Linux', '2026-07-27T09:20:00Z', NULL, NULL, 1);
INSERT INTO "push_subscriptions" ("id", "user_id", "endpoint", "p256dh", "auth", "ua_label", "created_at", "last_ok_at", "last_confirmed_at", "failures") VALUES (3, 2, 'https://push.example.org/f/dad-phone', 'BL8U3yRoOHM2', 'Zu7x4uMs', 'Android', '2026-07-27T09:20:00Z', NULL, NULL, 0);

-- notification_prefs (4 rows)
INSERT INTO "notification_prefs" ("user_id", "digest_enabled", "digest_time", "digest_on_empty", "daily_summary_mode", "summary_time", "email_reminders", "email_digest", "activity_push") VALUES (1, 1, '07:30', 0, 0, '20:00', 1, 0, 1);
INSERT INTO "notification_prefs" ("user_id", "digest_enabled", "digest_time", "digest_on_empty", "daily_summary_mode", "summary_time", "email_reminders", "email_digest", "activity_push") VALUES (2, 1, '06:45', 0, 0, '20:00', 1, 1, 1);
INSERT INTO "notification_prefs" ("user_id", "digest_enabled", "digest_time", "digest_on_empty", "daily_summary_mode", "summary_time", "email_reminders", "email_digest", "activity_push") VALUES (3, 0, '07:30', 0, 1, '21:15', 0, 0, 1);
INSERT INTO "notification_prefs" ("user_id", "digest_enabled", "digest_time", "digest_on_empty", "daily_summary_mode", "summary_time", "email_reminders", "email_digest", "activity_push") VALUES (4, 1, '08:00', 1, 0, '20:00', 1, 0, 0);

-- notification_queue (4 rows)
INSERT INTO "notification_queue" ("id", "user_id", "kind", "source_ref", "payload", "due_at", "sending_started_at", "sent_at", "skipped", "attempts") VALUES (1, 1, 'reminder', 'reminder:1:2026-07-27:1', '{"title":"Leo''s dentist","body":"Tomorrow at 16:30"}', '2026-07-26T14:30:00Z', NULL, '2026-07-27T06:00:00Z', NULL, 0);
INSERT INTO "notification_queue" ("id", "user_id", "kind", "source_ref", "payload", "due_at", "sending_started_at", "sent_at", "skipped", "attempts") VALUES (2, 2, 'digest', 'digest:2026-07-28', '{"day":"2026-07-28","count":2}', '2026-07-28T04:45:00Z', NULL, NULL, NULL, 0);
INSERT INTO "notification_queue" ("id", "user_id", "kind", "source_ref", "payload", "due_at", "sending_started_at", "sent_at", "skipped", "attempts") VALUES (3, 4, 'digest', 'digest:2026-07-27', '{"day":"2026-07-27","count":1}', '2026-07-27T06:00:00Z', '2026-07-27T08:00:00Z', NULL, 'stale', 0);
INSERT INTO "notification_queue" ("id", "user_id", "kind", "source_ref", "payload", "due_at", "sending_started_at", "sent_at", "skipped", "attempts") VALUES (4, 3, 'summary', 'summary:2026-07-27', '{"day":"2026-07-27","changes":3}', '2026-07-27T19:15:00Z', NULL, NULL, NULL, 0);

-- activity_log (11 rows)
INSERT INTO "activity_log" ("id", "calendar_id", "user_id", "action", "event_id", "title", "at") VALUES (1, 1, 1, 'event_created', 1, 'Leo''s dentist', '2026-07-27T09:10:00Z');
INSERT INTO "activity_log" ("id", "calendar_id", "user_id", "action", "event_id", "title", "at") VALUES (2, 1, 2, 'event_created', 2, 'Parents'' evening', '2026-07-27T09:11:00Z');
INSERT INTO "activity_log" ("id", "calendar_id", "user_id", "action", "event_id", "title", "at") VALUES (3, 1, 1, 'event_created', 3, 'Seaside holiday', '2026-07-27T09:12:00Z');
INSERT INTO "activity_log" ("id", "calendar_id", "user_id", "action", "event_id", "title", "at") VALUES (4, 3, 2, 'event_created', 4, 'Swimming', '2026-07-27T09:13:00Z');
INSERT INTO "activity_log" ("id", "calendar_id", "user_id", "action", "event_id", "title", "at") VALUES (5, 3, 2, 'event_updated', 5, 'Swimming (later than usual)', '2026-07-27T09:14:00Z');
INSERT INTO "activity_log" ("id", "calendar_id", "user_id", "action", "event_id", "title", "at") VALUES (6, 3, 2, 'event_deleted', 4, 'Swimming', '2026-07-27T09:15:00Z');
INSERT INTO "activity_log" ("id", "calendar_id", "user_id", "action", "event_id", "title", "at") VALUES (7, 1, 1, 'event_created', 6, 'Leo''s birthday', '2026-07-27T09:16:00Z');
INSERT INTO "activity_log" ("id", "calendar_id", "user_id", "action", "event_id", "title", "at") VALUES (8, 2, 1, 'event_created', 7, 'Household accounts', '2026-07-27T09:17:00Z');
INSERT INTO "activity_log" ("id", "calendar_id", "user_id", "action", "event_id", "title", "at") VALUES (9, 2, 2, 'event_created', 8, 'Cinema', '2026-07-27T09:18:00Z');
INSERT INTO "activity_log" ("id", "calendar_id", "user_id", "action", "event_id", "title", "at") VALUES (10, 3, 1, 'event_created', 9, 'Guitar lesson', '2026-07-27T09:19:00Z');
INSERT INTO "activity_log" ("id", "calendar_id", "user_id", "action", "event_id", "title", "at") VALUES (11, 1, 4, 'member_joined', NULL, 'Gran', '2026-07-27T09:20:00Z');

-- holiday_overrides (2 rows)
INSERT INTO "holiday_overrides" ("date", "name") VALUES ('2026-05-08', 'Victoire 1945 (jour de pont)');
INSERT INTO "holiday_overrides" ("date", "name") VALUES ('2026-11-11', NULL);

-- meta (3 rows)
INSERT INTO "meta" ("key", "value") VALUES ('planner_horizon', '2026-08-26');
INSERT INTO "meta" ("key", "value") VALUES ('scheduler_heartbeat', '2026-07-27T09:00:00Z');
INSERT INTO "meta" ("key", "value") VALUES ('last_backup', '{"at":"2026-07-27T03:15:00Z","bytes":167936,"ok":true}');
