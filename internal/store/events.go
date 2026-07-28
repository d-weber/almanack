package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"almanack/internal/domain"
)

const (
	eventColsBare = `id, calendar_id, title, all_day, starts_at, ends_at, start_date, end_date, ` +
		`location, url, notes, label_id, recurrence_id, created_by, created_at, updated_by, updated_at, event_uid`
	eventColsE = `e.id, e.calendar_id, e.title, e.all_day, e.starts_at, e.ends_at, e.start_date, e.end_date, ` +
		`e.location, e.url, e.notes, e.label_id, e.recurrence_id, e.created_by, e.created_at, e.updated_by, ` +
		`e.updated_at, e.event_uid`
)

func scanEvent(row rowScanner) (domain.Event, error) {
	var e domain.Event
	var recurrenceID sql.NullInt64
	err := row.Scan(
		&e.ID, &e.CalendarID, &e.Title, &e.AllDay,
		instantCol{&e.StartsAt}, instantCol{&e.EndsAt}, &e.StartDate, &e.EndDate,
		&e.Location, &e.URL, &e.Notes, &e.LabelID, &recurrenceID,
		&e.CreatedBy, instantCol{&e.CreatedAt}, &e.UpdatedBy, instantCol{&e.UpdatedAt}, &e.EventUID,
	)
	if err != nil {
		return domain.Event{}, mapErr(err)
	}
	e.RecurrenceID = i64ptr(recurrenceID)
	return e, nil
}

// eventValueArgs returns the eleven caller-supplied column values of an event, in the
// order the INSERT and UPDATE below use them.
//
// The time columns are passed through exactly as the caller set them rather than being
// filtered by e.AllDay. That is on purpose: the schema's CHECK constraint declares an
// all-day event with instants (or a timed event with dates) impossible, and mapErr
// turns that into domain.ErrInvalid. Silently dropping the contradictory half would
// store something the caller did not ask for and hide their bug.
func eventValueArgs(e domain.Event) []any {
	return []any{
		e.Title, searchNorm(e.Title, e.Location, e.Notes), boolArg(e.AllDay),
		putInstant(e.StartsAt), putInstant(e.EndsAt), e.StartDate, e.EndDate,
		e.Location, e.URL, e.Notes, e.LabelID,
	}
}

// CreateEvent inserts an event, optionally as the template of a new recurring series.
//
// When rec is non-nil the recurrence row is written first and linked, in the same
// transaction as the event and its participants — a series template with a dangling
// recurrence_id, or an event whose participants half-landed, is not a state this
// function can leave behind.
//
// The store assigns ID, CreatedAt, UpdatedAt and EventUID; whatever the caller left in
// those is ignored. UpdatedBy defaults to CreatedBy. A malformed event (all-day carrying
// instants, or timed carrying dates) is domain.ErrInvalid, straight from the schema's
// CHECK.
//
// The name is minted here, beside the timestamps, because it has exactly one job — to be
// unlike every other — and a caller is the wrong place to rely on for that. It is what
// the notification outbox files this event's reminders under, since id will not do:
// SQLite reissues the ids of deleted rows, and a reminder filed under a reused one is
// taken for the reminder already sent under it (migration 0007).
//
// crypto/rand.Text is 130 bits and has no error to return; the second half is what
// decides it. A household's decade of appointments needs nothing like that width,
// whereas an error here would have to travel up through the edit's transaction and fail
// the edit — an appointment that could not be saved because the machine was briefly
// short of randomness.
func (s *Store) CreateEvent(ctx context.Context, e domain.Event, rec *domain.Recurrence) (domain.Event, error) {
	now := s.now()
	if e.UpdatedBy == 0 {
		e.UpdatedBy = e.CreatedBy
	}
	participants := dedupeIDs(e.Participants)

	var out domain.Event
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if rec != nil {
			created, err := insertRecurrence(ctx, tx, *rec)
			if err != nil {
				return err
			}
			e.RecurrenceID = &created.ID
		}
		args := append(eventValueArgs(e),
			putInt64Ptr(e.RecurrenceID), e.CreatedBy, mustInstant(now), e.UpdatedBy, mustInstant(now), rand.Text())
		args = append([]any{e.CalendarID}, args...)

		var err error
		out, err = scanEvent(tx.QueryRowContext(ctx, `
			INSERT INTO events (calendar_id, title, search_norm, all_day, starts_at, ends_at, start_date, end_date,
			                    location, url, notes, label_id, recurrence_id, created_by, created_at, updated_by,
			                    updated_at, event_uid)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING `+eventColsBare, args...))
		if err != nil {
			return err
		}
		if err := setParticipants(ctx, tx, out.ID, participants); err != nil {
			return err
		}
		out.Participants = participants
		return nil
	})
	if err != nil {
		return domain.Event{}, fmt.Errorf("create event %q: %w", e.Title, err)
	}
	return out, nil
}

// EventByID returns one event with its participants, or domain.ErrNotFound.
func (s *Store) EventByID(ctx context.Context, id int64) (domain.Event, error) {
	e, err := scanEvent(s.q.QueryRowContext(ctx, `SELECT `+eventColsBare+` FROM events WHERE id = ?`, id))
	if err != nil {
		return domain.Event{}, fmt.Errorf("event %d: %w", id, err)
	}
	parts, err := listParticipants(ctx, s.q, id)
	if err != nil {
		return domain.Event{}, fmt.Errorf("event %d: %w", id, err)
	}
	e.Participants = parts
	return e, nil
}

// UpdateEvent saves an event in place and refreshes its search_norm.
//
// It does not touch participants (SetParticipants does), nor calendar_id, created_by
// or created_at, which are immutable. It does write recurrence_id, because that is how
// an event becomes or stops being a series template — and how the "this and following"
// split repoints the tail of a series.
func (s *Store) UpdateEvent(ctx context.Context, e domain.Event) error {
	args := append(eventValueArgs(e), putInt64Ptr(e.RecurrenceID), e.UpdatedBy, mustInstant(s.now()), e.ID)
	err := affected(s.q.ExecContext(ctx, `
		UPDATE events
		   SET title = ?, search_norm = ?, all_day = ?, starts_at = ?, ends_at = ?, start_date = ?, end_date = ?,
		       location = ?, url = ?, notes = ?, label_id = ?, recurrence_id = ?, updated_by = ?, updated_at = ?
		 WHERE id = ?`, args...))
	if err != nil {
		return fmt.Errorf("update event %d: %w", e.ID, err)
	}
	return nil
}

// DeleteEvent removes an event, cascading to its participants and reminders.
//
// Note the schema's cascade on event_overrides.override_event_id: deleting an event
// that some occurrence was overridden with deletes the override row too, which
// restores that occurrence to the series default. Cancelling an occurrence is a
// SetOverride with a nil event id, not a DeleteEvent.
func (s *Store) DeleteEvent(ctx context.Context, id int64) error {
	err := affected(s.q.ExecContext(ctx, `DELETE FROM events WHERE id = ?`, id))
	if err != nil {
		return fmt.Errorf("delete event %d: %w", id, err)
	}
	return nil
}

// SetParticipants replaces an event's participant set. An empty or nil slice clears it;
// duplicate ids are collapsed.
func (s *Store) SetParticipants(ctx context.Context, eventID int64, userIDs []int64) error {
	ids := dedupeIDs(userIDs)
	err := s.tx(ctx, func(tx *sql.Tx) error { return setParticipants(ctx, tx, eventID, ids) })
	if err != nil {
		return fmt.Errorf("set participants of event %d: %w", eventID, err)
	}
	return nil
}

func setParticipants(ctx context.Context, q querier, eventID int64, ids []int64) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM event_participants WHERE event_id = ?`, eventID); err != nil {
		return mapErr(err)
	}
	for _, id := range ids {
		if _, err := q.ExecContext(ctx,
			`INSERT INTO event_participants (event_id, user_id) VALUES (?, ?)`, eventID, id); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// ListParticipants returns the user ids attached to an event, ascending.
func (s *Store) ListParticipants(ctx context.Context, eventID int64) ([]int64, error) {
	ids, err := listParticipants(ctx, s.q, eventID)
	if err != nil {
		return nil, fmt.Errorf("participants of event %d: %w", eventID, err)
	}
	return ids, nil
}

func listParticipants(ctx context.Context, q querier, eventID int64) ([]int64, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT user_id FROM event_participants WHERE event_id = ? ORDER BY user_id`, eventID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// participantsFor loads the participants of many events in one query and returns them
// keyed by event id. EventsInRange uses it so that a month of events costs one query
// rather than one per event.
func participantsFor(ctx context.Context, q querier, eventIDs []int64) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx,
		`SELECT event_id, user_id FROM event_participants WHERE event_id IN (`+placeholders(len(eventIDs))+`)
		 ORDER BY event_id, user_id`, idArgs(eventIDs)...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventID, userID int64
		if err := rows.Scan(&eventID, &userID); err != nil {
			return nil, mapErr(err)
		}
		out[eventID] = append(out[eventID], userID)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Recurrences
// ---------------------------------------------------------------------------

const recurrenceCols = `id, freq, interval, by_weekday, by_monthday, week_ordinal, month_last_day, until, dtstart`

func scanRecurrence(row rowScanner) (domain.Recurrence, error) {
	var r domain.Recurrence
	var byWeekday sql.NullString
	var byMonthday, weekOrdinal sql.NullInt64
	err := row.Scan(&r.ID, &r.Freq, &r.Interval, &byWeekday, &byMonthday, &weekOrdinal,
		&r.MonthLastDay, datePtrCol{&r.Until}, &r.DTStart)
	if err != nil {
		return domain.Recurrence{}, mapErr(err)
	}
	r.ByMonthday = intptr(byMonthday)
	r.WeekOrdinal = intptr(weekOrdinal)
	wd, err := decodeWeekdays(byWeekday)
	if err != nil {
		return domain.Recurrence{}, err
	}
	r.ByWeekday = wd
	return r, nil
}

// encodeWeekdays stores a weekday set as a CSV of 0..6 with Sunday = 0, matching
// time.Weekday's own numbering so no translation table is needed on the way back out.
// The set is canonicalised (sorted, deduplicated) so two equal sets are byte-equal in
// the file.
//
// This numbering is storage only. Recurrence *math* uses WKST = Monday always
// (CONVENTIONS.md §4); the per-user week_start preference is display and never reaches
// internal/recur.
func encodeWeekdays(days []time.Weekday) any {
	if len(days) == 0 {
		return nil
	}
	seen := [7]bool{}
	for _, d := range days {
		if d >= time.Sunday && d <= time.Saturday {
			seen[d] = true
		}
	}
	var parts []string
	for d, ok := range seen {
		if ok {
			parts = append(parts, strconv.Itoa(d))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return strings.Join(parts, ",")
}

func decodeWeekdays(v sql.NullString) ([]time.Weekday, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	var out []time.Weekday
	for _, p := range strings.Split(v.String, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 6 {
			return nil, fmt.Errorf("decode by_weekday %q: %w", v.String, domain.ErrInvalid)
		}
		out = append(out, time.Weekday(n))
	}
	return out, nil
}

// CreateRecurrence inserts a standalone recurrence. Creating a series normally goes
// through CreateEvent, which does this and the event together; this is for the
// series-split path, which needs the new recurrence before it has the new template.
func (s *Store) CreateRecurrence(ctx context.Context, r domain.Recurrence) (domain.Recurrence, error) {
	out, err := insertRecurrence(ctx, s.q, r)
	if err != nil {
		return domain.Recurrence{}, fmt.Errorf("create recurrence: %w", err)
	}
	return out, nil
}

func insertRecurrence(ctx context.Context, q querier, r domain.Recurrence) (domain.Recurrence, error) {
	if r.Interval <= 0 {
		r.Interval = 1
	}
	var until any
	if r.Until != nil {
		until = *r.Until
	}
	return scanRecurrence(q.QueryRowContext(ctx, `
		INSERT INTO recurrences (freq, interval, by_weekday, by_monthday, week_ordinal, month_last_day, until, dtstart)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING `+recurrenceCols,
		string(r.Freq), r.Interval, encodeWeekdays(r.ByWeekday), putIntPtr(r.ByMonthday),
		putIntPtr(r.WeekOrdinal), boolArg(r.MonthLastDay), until, r.DTStart))
}

// RecurrenceByID returns one recurrence, or domain.ErrNotFound.
func (s *Store) RecurrenceByID(ctx context.Context, id int64) (domain.Recurrence, error) {
	r, err := scanRecurrence(s.q.QueryRowContext(ctx, `SELECT `+recurrenceCols+` FROM recurrences WHERE id = ?`, id))
	if err != nil {
		return domain.Recurrence{}, fmt.Errorf("recurrence %d: %w", id, err)
	}
	return r, nil
}

// UpdateRecurrence saves a repeat pattern. Shortening a series by setting Until does
// not delete the overrides beyond the new end; they simply stop being reachable, and
// come back if the series is later extended.
func (s *Store) UpdateRecurrence(ctx context.Context, r domain.Recurrence) error {
	if r.Interval <= 0 {
		r.Interval = 1
	}
	var until any
	if r.Until != nil {
		until = *r.Until
	}
	err := affected(s.q.ExecContext(ctx, `
		UPDATE recurrences
		   SET freq = ?, interval = ?, by_weekday = ?, by_monthday = ?, week_ordinal = ?,
		       month_last_day = ?, until = ?, dtstart = ?
		 WHERE id = ?`,
		string(r.Freq), r.Interval, encodeWeekdays(r.ByWeekday), putIntPtr(r.ByMonthday),
		putIntPtr(r.WeekOrdinal), boolArg(r.MonthLastDay), until, r.DTStart, r.ID))
	if err != nil {
		return fmt.Errorf("update recurrence %d: %w", r.ID, err)
	}
	return nil
}

// DeleteRecurrence drops a repeat pattern. Its overrides cascade away; the template
// event survives with recurrence_id set to NULL (ON DELETE SET NULL), becoming a plain
// one-off event. That is what "stop repeating this from now on" leaves behind.
func (s *Store) DeleteRecurrence(ctx context.Context, id int64) error {
	err := affected(s.q.ExecContext(ctx, `DELETE FROM recurrences WHERE id = ?`, id))
	if err != nil {
		return fmt.Errorf("delete recurrence %d: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Overrides
// ---------------------------------------------------------------------------

// Overrides returns a series' exceptions, keyed by the family-tz date of the original
// occurrence. A nil value means that occurrence is cancelled; a non-nil value is the id
// of the standalone event carrying the edited version.
func (s *Store) Overrides(ctx context.Context, recurrenceID int64) (map[domain.Date]*int64, error) {
	out, err := overridesFor(ctx, s.q, []int64{recurrenceID})
	if err != nil {
		return nil, fmt.Errorf("overrides of recurrence %d: %w", recurrenceID, err)
	}
	m := out[recurrenceID]
	if m == nil {
		m = map[domain.Date]*int64{}
	}
	return m, nil
}

func overridesFor(ctx context.Context, q querier, recurrenceIDs []int64) (map[int64]map[domain.Date]*int64, error) {
	out := map[int64]map[domain.Date]*int64{}
	if len(recurrenceIDs) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT recurrence_id, occurrence_date, override_event_id
		  FROM event_overrides
		 WHERE recurrence_id IN (`+placeholders(len(recurrenceIDs))+`)`, idArgs(recurrenceIDs)...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var recID int64
		var d domain.Date
		var evID sql.NullInt64
		if err := rows.Scan(&recID, &d, &evID); err != nil {
			return nil, mapErr(err)
		}
		if out[recID] == nil {
			out[recID] = map[domain.Date]*int64{}
		}
		out[recID][d] = i64ptr(evID)
	}
	return out, rows.Err()
}

// SetOverride records an exception for one occurrence of a series, replacing any
// existing one for that date. A nil overrideEventID cancels the occurrence; otherwise
// it points at the standalone event holding the edited values. Idempotent.
//
// date is the family-tz date of the occurrence *in the original series*, which is what
// makes the identity stable when the edited copy is moved to another day.
func (s *Store) SetOverride(ctx context.Context, recurrenceID int64, date domain.Date, overrideEventID *int64) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO event_overrides (recurrence_id, occurrence_date, override_event_id)
		VALUES (?, ?, ?)
		ON CONFLICT (recurrence_id, occurrence_date)
		DO UPDATE SET override_event_id = excluded.override_event_id`,
		recurrenceID, date, putInt64Ptr(overrideEventID))
	if err != nil {
		return fmt.Errorf("set override of recurrence %d on %s: %w", recurrenceID, date, mapErr(err))
	}
	return nil
}

// DeleteOverride restores an occurrence to the series default. Idempotent: the
// contract is "this occurrence has no exception", and it is already satisfied when
// there was none.
//
// It does not delete the event an override pointed at; the caller decides whether that
// edited copy should become a standalone event or be deleted.
func (s *Store) DeleteOverride(ctx context.Context, recurrenceID int64, date domain.Date) error {
	_, err := s.q.ExecContext(ctx,
		`DELETE FROM event_overrides WHERE recurrence_id = ? AND occurrence_date = ?`, recurrenceID, date)
	if err != nil {
		return fmt.Errorf("delete override of recurrence %d on %s: %w", recurrenceID, date, mapErr(err))
	}
	return nil
}

// OverrideRef says which occurrence of which series an edited copy stands for.
type OverrideRef struct {
	RecurrenceID int64
	// SeriesEventID is the template event of that series — the id every scoped edit
	// is expressed in terms of.
	SeriesEventID int64
	// OccurrenceDate is the family-tz date of the occurrence in the original series,
	// which is the identity the override is filed under and survives the copy being
	// moved to another day.
	OccurrenceDate domain.Date
}

// OverrideRefByEventID resolves an event id back to the occurrence it overrides, if it
// overrides one.
//
// Editing a single occurrence stores a standalone copy of the event, and that copy's
// own id is what the API publishes and the client sends back. Every later request about
// that occurrence therefore names the copy rather than the series — and a copy carries
// no recurrence_id of its own, so without this lookup it is indistinguishable from an
// ordinary one-off event and the edit scope and date go unread.
//
// Returns domain.ErrNotFound when the id is an ordinary event, which includes the copy
// a series split deliberately detached: that one is a standalone event now, and saying
// otherwise would attach it to a series that no longer produces its date.
func (s *Store) OverrideRefByEventID(ctx context.Context, eventID int64) (OverrideRef, error) {
	var ref OverrideRef
	// The join is what finds the template: a series has exactly one, and an override
	// copy never has a recurrence_id, so it cannot match itself. ORDER BY keeps the
	// answer stable rather than trusting the query plan for a row set the schema does
	// not constrain to one.
	err := s.q.QueryRowContext(ctx, `
		SELECT o.recurrence_id, o.occurrence_date, t.id
		  FROM event_overrides o
		  JOIN events t ON t.recurrence_id = o.recurrence_id
		 WHERE o.override_event_id = ?
		 ORDER BY o.occurrence_date, t.id
		 LIMIT 1`, eventID).Scan(&ref.RecurrenceID, &ref.OccurrenceDate, &ref.SeriesEventID)
	if err != nil {
		return OverrideRef{}, fmt.Errorf("series of override event %d: %w", eventID, mapErr(err))
	}
	return ref, nil
}

// RepointOverrides moves every override on or after fromDate from one series to
// another. It is the second half of the "this and following occurrences" edit: the
// original series is capped with UNTIL, a new series takes over from fromDate, and the
// exceptions the family had already made to those future occurrences have to travel
// with them or they are silently lost.
//
// Idempotent, and a no-op when there is nothing to move.
func (s *Store) RepointOverrides(ctx context.Context, fromRecurrence, toRecurrence int64, fromDate domain.Date) error {
	if fromDate.IsZero() {
		// A zero date would bind as NULL, every comparison against it would be NULL,
		// and the update would quietly move nothing.
		return fmt.Errorf("repoint overrides from recurrence %d: split date must be set: %w",
			fromRecurrence, domain.ErrInvalid)
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE event_overrides
		   SET recurrence_id = ?
		 WHERE recurrence_id = ? AND occurrence_date >= ?`,
		toRecurrence, fromRecurrence, fromDate)
	if err != nil {
		return fmt.Errorf("repoint overrides from recurrence %d to %d at %s: %w",
			fromRecurrence, toRecurrence, fromDate, mapErr(err))
	}
	return nil
}

// ---------------------------------------------------------------------------
// The range read
// ---------------------------------------------------------------------------

// RangeResult is everything needed to draw a window of the calendar. It is deliberately
// a complete answer: internal/recur expands the series from this struct alone, with no
// further trip to the database, so rendering a month is a fixed small number of
// queries no matter how many occurrences it contains.
type RangeResult struct {
	// Singles are the non-recurring events overlapping the window, timed and all-day
	// alike. Events that exist only as a series override are not here — they arrive
	// inside their SeriesRow.
	Singles []domain.Event
	// Series are the recurring series that could produce an occurrence in the window.
	Series []SeriesRow
}

// SeriesRow is one recurring series and the exceptions that apply to it.
type SeriesRow struct {
	// Event is the series template: the title, times and duration every occurrence
	// inherits unless an override says otherwise.
	Event domain.Event
	// Recurrence is the repeat pattern.
	Recurrence domain.Recurrence
	// Overrides maps the family-tz date of an original occurrence to its exception:
	// nil means cancelled, otherwise the id of the event in OverrideEvents.
	//
	// This holds *all* of the series' overrides, not only those dated inside the
	// window, because an override may move an occurrence into the window from a date
	// outside it. Filtering here would make those occurrences disappear.
	Overrides map[domain.Date]*int64
	// OverrideEvents holds the edited copies, keyed by event id.
	OverrideEvents map[int64]domain.Event
}

// seriesTailDays is how far before the window a series may have ended and still be
// read back.
//
// An occurrence that starts before the window can still reach into it — a three-day
// trip beginning on the last day of the series is the ordinary case — and how far it
// reaches depends on the event, not on the pattern. Working that out in SQL would mean
// doing family-tz day arithmetic over two different column pairs, which is the
// off-by-one-day bug this schema exists to prevent. So the query is deliberately a
// coarse filter: too wide costs a handful of rows that expansion then discards, too
// narrow loses events, and only one of those is a bug. Expansion in internal/events is
// the exact test, and it already reaches back by the event's own span.
//
// A month of tail covers every occurrence a family plausibly creates. One longer than
// that, belonging to a series that has also ended, is still missed; that is a known
// limit rather than an oversight.
const seriesTailDays = 31

// EventsInRange returns everything visible in the window [from, to] (both inclusive,
// interpreted as family-tz dates) across the given calendars.
//
// An all-day event overlaps when its inclusive date span meets the window. A timed
// event overlaps when its instants meet the window converted to instants in the family
// timezone — which is why the store holds a *time.Location: doing this comparison in
// UTC is the off-by-one-day bug this schema exists to prevent.
//
// A series is included when it could plausibly produce an occurrence in the window:
// dtstart <= to and it had not ended more than seriesTailDays before from, or it has an
// override, which can put an occurrence anywhere at all. Whether it actually produces
// one is internal/recur's business, and it has everything it needs to decide.
//
// Participants are loaded for every event returned, templates and override copies
// included.
//
// All five statements run inside one read transaction, so what comes back is the
// database at a single instant rather than at five of them. Without it, an edit
// committing between the singles query and the overrides query is observed half-applied:
// the same occurrence drawn twice, once as itself and once inside its series. See
// Store.readTx for why that transaction costs no write lock.
func (s *Store) EventsInRange(ctx context.Context, calendarIDs []int64, from, to domain.Date) (RangeResult, error) {
	var res RangeResult
	if len(calendarIDs) == 0 {
		return res, nil
	}
	if from.IsZero() || to.IsZero() {
		return res, fmt.Errorf("events in range: window bounds must be set: %w", domain.ErrInvalid)
	}
	if to.Before(from) {
		return res, fmt.Errorf("events in range: %s is before %s: %w", to, from, domain.ErrInvalid)
	}

	// The half-open instant window matching the inclusive date window.
	fromTS := mustInstant(from.In(s.loc))
	toTS := mustInstant(to.AddDays(1).In(s.loc))

	calArgs := idArgs(calendarIDs)
	calIn := placeholders(len(calendarIDs))

	err := s.readTx(ctx, func(q querier) error {
		// Singles. The NOT EXISTS keeps override copies out: they are ordinary rows with
		// a NULL recurrence_id, and without it every edited occurrence would be drawn
		// twice, once as itself and once inside its series.
		singleArgs := append(append([]any{}, calArgs...), to, from, toTS, fromTS, fromTS)
		rows, err := q.QueryContext(ctx, `
			SELECT `+eventColsE+`
			  FROM events e
			 WHERE e.calendar_id IN (`+calIn+`)
			   AND e.recurrence_id IS NULL
			   AND NOT EXISTS (SELECT 1 FROM event_overrides o WHERE o.override_event_id = e.id)
			   AND ( (e.all_day = 1 AND e.start_date <= ? AND e.end_date >= ?)
			      OR (e.all_day = 0 AND e.starts_at < ? AND (e.ends_at > ? OR e.starts_at >= ?)) )
			 ORDER BY COALESCE(e.start_date, e.starts_at), e.id`, singleArgs...)
		if err != nil {
			return mapErr(err)
		}
		singles, err := collectEvents(rows)
		if err != nil {
			return err
		}

		// Series templates joined to their pattern.
		//
		// The EXISTS is not an optimisation: an override can move an occurrence outside
		// the series' own dtstart..until envelope, in either direction, and the copy
		// carrying it is kept out of the singles query above by design. A series dropped
		// here therefore takes that occurrence with it — the last lesson of term, dragged
		// into the following month, simply stops existing. Only a copy can do that; a
		// cancellation removes an occurrence and can never make one appear, so it does
		// not widen anything.
		seriesArgs := append(append([]any{}, calArgs...), to, from.AddDays(-seriesTailDays))
		rows, err = q.QueryContext(ctx, `
			SELECT `+eventColsE+`, `+prefixedRecurrenceCols+`
			  FROM events e
			  JOIN recurrences r ON r.id = e.recurrence_id
			 WHERE e.calendar_id IN (`+calIn+`)
			   AND ( (r.dtstart <= ? AND (r.until IS NULL OR r.until >= ?))
			      OR EXISTS (SELECT 1 FROM event_overrides o
			                  WHERE o.recurrence_id = r.id AND o.override_event_id IS NOT NULL) )
			 ORDER BY r.dtstart, e.id`, seriesArgs...)
		if err != nil {
			return mapErr(err)
		}
		series, err := collectSeries(rows)
		if err != nil {
			return err
		}

		recurrenceIDs := make([]int64, 0, len(series))
		for i := range series {
			recurrenceIDs = append(recurrenceIDs, series[i].Recurrence.ID)
		}
		overrides, err := overridesFor(ctx, q, recurrenceIDs)
		if err != nil {
			return err
		}

		var overrideIDs []int64
		for i := range series {
			m := overrides[series[i].Recurrence.ID]
			if m == nil {
				m = map[domain.Date]*int64{}
			}
			series[i].Overrides = m
			for _, id := range m {
				if id != nil {
					overrideIDs = append(overrideIDs, *id)
				}
			}
		}

		overrideEvents, err := eventsByID(ctx, q, dedupeIDs(overrideIDs))
		if err != nil {
			return err
		}
		for i := range series {
			series[i].OverrideEvents = map[int64]domain.Event{}
			for _, id := range series[i].Overrides {
				if id == nil {
					continue
				}
				if ev, ok := overrideEvents[*id]; ok {
					series[i].OverrideEvents[*id] = ev
				}
			}
		}

		// One participant query for the whole result.
		allIDs := make([]int64, 0, len(singles)+len(series)+len(overrideEvents))
		for i := range singles {
			allIDs = append(allIDs, singles[i].ID)
		}
		for i := range series {
			allIDs = append(allIDs, series[i].Event.ID)
		}
		for id := range overrideEvents {
			allIDs = append(allIDs, id)
		}
		parts, err := participantsFor(ctx, q, dedupeIDs(allIDs))
		if err != nil {
			return err
		}
		for i := range singles {
			singles[i].Participants = parts[singles[i].ID]
		}
		for i := range series {
			series[i].Event.Participants = parts[series[i].Event.ID]
			for id, ev := range series[i].OverrideEvents {
				ev.Participants = parts[id]
				series[i].OverrideEvents[id] = ev
			}
		}

		res.Singles = singles
		res.Series = series
		return nil
	})
	if err != nil {
		return RangeResult{}, fmt.Errorf("events in range: %w", err)
	}
	return res, nil
}

// prefixedRecurrenceCols is recurrenceCols qualified for the join in EventsInRange.
const prefixedRecurrenceCols = `r.id, r.freq, r.interval, r.by_weekday, r.by_monthday, r.week_ordinal, ` +
	`r.month_last_day, r.until, r.dtstart`

func collectEvents(rows *sql.Rows) ([]domain.Event, error) {
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func collectSeries(rows *sql.Rows) ([]SeriesRow, error) {
	defer rows.Close()
	var out []SeriesRow
	for rows.Next() {
		var e domain.Event
		var recurrenceID sql.NullInt64
		var r domain.Recurrence
		var byWeekday sql.NullString
		var byMonthday, weekOrdinal sql.NullInt64
		err := rows.Scan(
			&e.ID, &e.CalendarID, &e.Title, &e.AllDay,
			instantCol{&e.StartsAt}, instantCol{&e.EndsAt}, &e.StartDate, &e.EndDate,
			&e.Location, &e.URL, &e.Notes, &e.LabelID, &recurrenceID,
			&e.CreatedBy, instantCol{&e.CreatedAt}, &e.UpdatedBy, instantCol{&e.UpdatedAt}, &e.EventUID,
			&r.ID, &r.Freq, &r.Interval, &byWeekday, &byMonthday, &weekOrdinal,
			&r.MonthLastDay, datePtrCol{&r.Until}, &r.DTStart,
		)
		if err != nil {
			return nil, mapErr(err)
		}
		e.RecurrenceID = i64ptr(recurrenceID)
		r.ByMonthday = intptr(byMonthday)
		r.WeekOrdinal = intptr(weekOrdinal)
		if r.ByWeekday, err = decodeWeekdays(byWeekday); err != nil {
			return nil, err
		}
		out = append(out, SeriesRow{Event: e, Recurrence: r})
	}
	return out, rows.Err()
}

// eventsByID loads a set of events in one query, keyed by id. Like overridesFor and
// participantsFor it takes the querier rather than reading s.q, so that EventsInRange can
// run it inside its read transaction.
func eventsByID(ctx context.Context, q querier, ids []int64) (map[int64]domain.Event, error) {
	out := map[int64]domain.Event{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx,
		`SELECT `+eventColsBare+` FROM events WHERE id IN (`+placeholders(len(ids))+`)`, idArgs(ids)...)
	if err != nil {
		return nil, mapErr(err)
	}
	events, err := collectEvents(rows)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		out[e.ID] = e
	}
	return out, nil
}

// searchLimit caps SearchEvents. A family calendar cannot plausibly want more than
// this from a text box, and an unbounded query is how a search field becomes a way to
// pull the whole database through the JSON encoder.
const searchLimit = 200

// SearchEvents finds events by free text across the given calendars, accent- and
// case-insensitively: both the stored search_norm and the query go through the same
// folding, so "ecole" matches "École" and vice versa.
//
// Search answers "find the thing I typed", and that decides how a series is treated.
// Normally a series is represented by its template, once: the edited copies standing in
// for single occurrences are occurrence-level edits of an event already in the results,
// and returning those too would show one series once per exception it has. But a copy
// can carry words its template does not — renaming a single occurrence is the ordinary
// way that happens — and then the template is not a hit at all, so hiding the copy makes
// the title the family actually typed unfindable. Hence the rule: a copy is returned
// exactly when it matches the text and its own template does not. On the text alone that
// is "no duplicates" and "no silent misses" in one condition. When no text is given, a
// participant or label filter alone still sees a series as its template once, because a
// template not being asked about text always counts as matching.
//
// The suppression looks at the template's text and nothing else, so a copy is hidden by a
// matching template even when a participant or label filter has excluded that template
// from the results — rename one occurrence and re-file it under a different label, search
// for the shared word with that label selected, and neither row comes back. Adding the
// filters to the subquery would fix it; it is left because a filter and a rename landing
// on the same occurrence is rare, and because the subquery is what keeps this query
// indexed. Pre-existing: the same predicate on the no-text path has always behaved this way.
//
// Three related limits, deliberately left. The first two need the dates of individual
// occurrences, which come from internal/recur and SQL cannot call:
//
//   - There is no date predicate. A series matches whether or not it still runs, and a
//     one-off matches whatever year it happened in. For a search box that is the point;
//     narrowing it would mean the caller naming a window, and nothing here has one.
//   - Rows are ordered newest first by their own date, which for a series template is
//     the date its pattern began rather than the date of any occurrence that matched: a
//     lesson happening next week sorts under 2019. Override copies do have a date of
//     their own and sort on it. Ordering templates by their next occurrence would mean
//     expanding every match, which internal/httpapi is already positioned to do — it
//     computes next_occurrence per result for display.
//   - Copies compete with everything else for the searchLimit rows, and sort on their own
//     dates, which are usually later than a template's. So a series with more matching
//     exceptions than that limit would fill the page and push out other events that used
//     to be on it. That takes 200 renamed occurrences of one series to reach, which is not
//     a household calendar, and capping copies per series costs a window function on every
//     search to buy nothing anyone will hit.
//
// participant and labelID are optional filters; nil means "no filter". Results are
// newest first and capped at searchLimit.
func (s *Store) SearchEvents(ctx context.Context, calendarIDs []int64, q string, participant *int64, labelID *int64) ([]domain.Event, error) {
	if len(calendarIDs) == 0 {
		return nil, nil
	}

	conds := []string{`e.calendar_id IN (` + placeholders(len(calendarIDs)) + `)`}
	args := idArgs(calendarIDs)

	// The join finds a copy's template: a series has exactly one event carrying its
	// recurrence_id and a copy never carries one, so a copy cannot suppress itself. A
	// copy whose template has been deleted matches nothing here and is kept, which is
	// right — there is no series left to stand for it.
	if needle := foldSearch(strings.TrimSpace(q)); needle == "" {
		conds = append(conds, `NOT EXISTS (SELECT 1 FROM event_overrides o WHERE o.override_event_id = e.id)`)
	} else {
		like := "%" + likeEscape(needle) + "%"
		conds = append(conds,
			`NOT EXISTS (SELECT 1 FROM event_overrides o
			              JOIN events t ON t.recurrence_id = o.recurrence_id
			             WHERE o.override_event_id = e.id
			               AND t.search_norm LIKE ? ESCAPE '\')`,
			`e.search_norm LIKE ? ESCAPE '\'`)
		args = append(args, like, like)
	}
	if participant != nil {
		conds = append(conds, `EXISTS (SELECT 1 FROM event_participants p WHERE p.event_id = e.id AND p.user_id = ?)`)
		args = append(args, *participant)
	}
	if labelID != nil {
		conds = append(conds, `e.label_id = ?`)
		args = append(args, *labelID)
	}
	args = append(args, searchLimit)

	rows, err := s.q.QueryContext(ctx, `
		SELECT `+eventColsE+`
		  FROM events e
		 WHERE `+strings.Join(conds, " AND ")+`
		 ORDER BY COALESCE(e.start_date, e.starts_at) DESC, e.id DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("search events: %w", mapErr(err))
	}
	events, err := collectEvents(rows)
	if err != nil {
		return nil, fmt.Errorf("search events: %w", err)
	}

	ids := make([]int64, 0, len(events))
	for i := range events {
		ids = append(ids, events[i].ID)
	}
	parts, err := participantsFor(ctx, s.q, ids)
	if err != nil {
		return nil, fmt.Errorf("search events: %w", err)
	}
	for i := range events {
		events[i].Participants = parts[events[i].ID]
	}
	return events, nil
}
