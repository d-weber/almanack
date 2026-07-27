-- Let a member say "no reminder, just for this one" about an occurrence they edited.
--
-- Editing one occurrence of a series leaves a standalone copy of the event behind, and
-- the question this table answers is what that copy means for reminders. The rule is
-- that an occurrence *inherits* its series' reminders until somebody changes them on
-- that occurrence: the planner reads the series' list for a date that has a copy, unless
-- this table says that member has set their own there, in which case it reads the copy's
-- and only the copy's. Never both — that is what announced one swimming lesson twice.
--
-- A row is written when a member saves a reminder list against the copy, *including an
-- empty one*. That is the whole reason the table exists: "no rows" already meant
-- "nothing has ever been set here", so it could not also mean "cleared on purpose", and
-- for want of somewhere to write the difference, taking the reminder off a single
-- occurrence used to show an empty list and go off anyway.
--
-- The alternative — copying the series' reminders onto the copy when the copy is created
-- — was tried first and drops reminders silently, which for this application is the
-- worst thing a change can do. Rows are copied at that one moment and never again, so a
-- reminder the series gains afterwards, or the first reminder a member sets after
-- joining the calendar, never reaches an occurrence somebody had already moved. The
-- family is not told; nothing on screen distinguishes that occurrence from any other.
--
-- Schema only. Nothing is dropped, nothing is rebuilt, no existing row is read, written
-- or rewritten, so this cannot fail on a database whatever state it is in and cannot
-- lose anything: an upgrade either creates one empty table or does not run at all. What
-- this migration does *not* claim is that a downgrade works. The 0.2.0 binary refuses to
-- open a database migrated past what it knows, by design and by name — "database schema
-- is at version 5 but this binary only knows 2: refusing to start" — so going back to it
-- means restoring a backup, not flipping a symlink. What is true is the part this file
-- is responsible for: it leaves every table that binary reads and writes exactly as it
-- was, so the backup taken before the upgrade and the file after it hold the same data.
--
-- A row here is only ever consulted for an event that is still an override copy. If a
-- "this and following" edit later detaches the copy from its series, the copy becomes an
-- ordinary event announced by its own reminders and this table is not asked about it;
-- the row goes when the event does.
CREATE TABLE reminder_detachments (
    event_id INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    user_id  INTEGER NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    PRIMARY KEY (event_id, user_id)
);
