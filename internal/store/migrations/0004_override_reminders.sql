-- Give the occurrences somebody has already edited their own copy of the reminders.
--
-- An edited occurrence owns its reminders: the copy an edit leaves behind carries them,
-- the editor lists them from there, and the planner fires those and not the series' for
-- that date. Until now the planner inherited the series' reminders for an edited
-- occurrence *as well as* using the copy's, which is how one lesson could be announced
-- twice and how a reminder somebody had taken off a single occurrence went off anyway.
--
-- Copies written before that rule existed have no reminders of their own, so under it
-- they would go quiet: the family would simply stop being reminded about the one lesson
-- they had moved, with nothing to say so. This gives each of them what the rule says
-- they should have had all along — a copy, per member, of the series' reminders.
--
-- It is deliberately per member and per copy: a member who already has reminders on a
-- copy is left alone, because those are the ones the editor showed them, and every other
-- member gets the series' rather than nobody's. The result is that everyone who is
-- reminded about an edited occurrence today is still reminded about it afterwards,
-- exactly once. A member who had deliberately cleared their reminders on one occurrence
-- gets them back, which is the one thing here that is not a faithful translation: until
-- now clearing them did nothing at all — the series fired anyway — so this restores what
-- they have been receiving, and from now on clearing them will work.
--
-- Adding rows is expand-only in the sense that matters: no table is rebuilt, no column
-- or table is dropped, and every statement the previous binary issues against reminders
-- names its columns, so it reads and writes this file unchanged. What that binary would
-- do with the new rows is announce those occurrences twice — which is the bug being
-- fixed here, on rows that previously had only one source for it. A rollback across this
-- migration is therefore noisier than the release it rolls back to; it is not broken.
--
-- The pairs that already have reminders of their own are collected once with DISTINCT
-- and joined, rather than tested row by row with a correlated NOT EXISTS against the
-- table being written. Whether a subquery may observe its own statement's inserts is not
-- something to depend on: a member with two reminders on the series would otherwise
-- risk having the first copied, being judged to have one already, and losing the second.
INSERT INTO reminders (event_id, recurrence_id, user_id, offset_minutes, days_before, at_time_local)
SELECT o.override_event_id, NULL, r.user_id, r.offset_minutes, r.days_before, r.at_time_local
  FROM event_overrides o
  JOIN reminders r ON r.recurrence_id = o.recurrence_id
  LEFT JOIN (SELECT DISTINCT event_id, user_id FROM reminders WHERE event_id IS NOT NULL) own
         ON own.event_id = o.override_event_id AND own.user_id = r.user_id
 WHERE o.override_event_id IS NOT NULL
   AND own.event_id IS NULL;
