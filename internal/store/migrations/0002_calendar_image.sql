-- Calendar cover images.
--
-- A family picks a calendar out of a list by picture faster than by name, and the
-- sidebar shows all of them at once. The image is stored in the row, like avatars,
-- so a backup is still one file with nothing to lose alongside it.
--
-- Adding a nullable column is an expand-only change: the previous binary keeps
-- running against this schema unchanged, which is what makes a rollback a matter of
-- putting the old binary back.
ALTER TABLE calendars ADD COLUMN image BLOB;
