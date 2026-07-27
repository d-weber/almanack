-- Re-fold the search index for the letters the fold table just learned.
--
-- events.search_norm is computed once, when an event is written, and the search box
-- folds the query the same way on the way in. So the two sides have to agree about
-- every rune, and adding one to the table breaks that agreement for rows already
-- stored: the query for "Søren" now folds to "soren" while the row still reads
-- "søren", and neither spelling matches any more.
--
-- That is worse than the gap it was meant to close. Before this release both sides
-- left "ø" alone, so typing the letter itself found the event; the table change alone
-- would have taken that away and given nothing back — a birthday filed under "Søren"
-- findable by nothing at all. Hence this file: it is not a nicety, it is the other
-- half of the change, and without it 0.3 would lose a search that 0.2.0 answered.
--
-- The rewrite is exact rather than approximate, which is why it can be a substitution
-- rather than a re-derivation from title, location and notes. searchNorm lowercases
-- before folding, and Go's strings.ToLower already mapped Ø ẞ Ð Þ Đ onto ø ß ð þ đ in
-- 0.2.0 — so a stored norm can only ever hold the lowercase forms, and replacing those
-- five produces exactly the bytes searchNorm would produce today. That equivalence is
-- pinned by TestBackfillMatchesSearchNorm rather than asserted here.
--
-- No substitution feeds another — every replacement is ASCII — so the order of the
-- replace() calls does not matter. The WHERE compares the row against its own rewrite
-- instead of matching a character class, because GLOB's treatment of multi-byte runes
-- inside brackets is not something to stake a family's search box on; this way the
-- statement is its own proof, and rows without any of the five are left untouched.
--
-- Rows only, no schema: nothing is dropped, nothing is rebuilt, and the previous
-- binary reads and writes this column exactly as before. It is idempotent — running it
-- twice is a no-op, since after the first pass no row differs from its own rewrite.
UPDATE events
   SET search_norm = replace(replace(replace(replace(replace(
         search_norm, 'ø', 'o'), 'ß', 'ss'), 'ð', 'd'), 'þ', 'th'), 'đ', 'd')
 WHERE search_norm <> replace(replace(replace(replace(replace(
         search_norm, 'ø', 'o'), 'ß', 'ss'), 'ð', 'd'), 'þ', 'th'), 'đ', 'd');
