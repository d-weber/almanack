package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"almanack/internal/domain"
)

const (
	// image is selected as a boolean rather than as bytes: the list of calendars is
	// read on every page load, and shipping the cover images through it would make
	// the commonest query the heaviest one. The bytes have their own endpoint.
	calendarColsBare = `id, name, color, creator_id, created_at, image IS NOT NULL`
	calendarColsC    = `c.id, c.name, c.color, c.creator_id, c.created_at, c.image IS NOT NULL`
)

func scanCalendar(row rowScanner) (domain.Calendar, error) {
	var c domain.Calendar
	if err := row.Scan(&c.ID, &c.Name, &c.Color, &c.CreatorID, instantCol{&c.CreatedAt}, &c.HasImage); err != nil {
		return domain.Calendar{}, mapErr(err)
	}
	return c, nil
}

// defaultLabels is the palette every new calendar is seeded with: exactly
// domain.LabelsPerCalendar rows, in ten hues spread around the wheel rather than
// merely different, so that they stay separable side by side on a month grid and for
// the commonest colour-vision deficiencies.
//
// Each label is named after its own colour, and that is the point. A label starts life
// as a colour and nothing else; what it *means* — school, work, the dog — is something
// a group decides for itself and types in. Naming them "Sky blue" rather than "Work"
// says so honestly, and the names are worth a second word each ("Poppy red", not "Red")
// because a list of ten flat colour words reads like a form to fill in rather than a
// palette to pick from. It also sidesteps the translation problem a guessed-at meaning
// creates: baking a language in at creation time would freeze every label in whichever
// one the person who made the calendar happened to read, and translating at display
// time would mean overwriting a name someone had chosen.
//
// Labels are never created or deleted afterwards — only renamed, recoloured and
// reordered. That is what lets events.label_id be NOT NULL forever, so no event can
// end up with no colour.
var defaultLabels = [domain.LabelsPerCalendar]struct {
	Name  string
	Color string
}{
	{"Emerald green", "#2ecc87"},
	{"Lagoon teal", "#3dc2c8"},
	{"Sky blue", "#47b2f7"},
	{"Warm taupe", "#948078"},
	{"Midnight black", "#212121"},
	{"Poppy red", "#e73b3b"},
	{"Raspberry pink", "#f35f8c"},
	{"Sunset coral", "#fb7f77"},
	{"Golden amber", "#fdc02d"},
	{"Soft lilac", "#b38bdc"},
}

// CreateCalendar creates a calendar and, in the same transaction, the two things a
// calendar is useless without: a membership row for its creator, and the ten labels.
// Seeding them here rather than lazily is what makes "every event has a label" true
// from the first millisecond.
//
// The store assigns ID and CreatedAt; those fields of c are ignored.
func (s *Store) CreateCalendar(ctx context.Context, c domain.Calendar) (domain.Calendar, error) {
	now := mustInstant(s.now())
	var out domain.Calendar
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = scanCalendar(tx.QueryRowContext(ctx, `
			INSERT INTO calendars (name, color, creator_id, created_at)
			VALUES (?, ?, ?, ?)
			RETURNING `+calendarColsBare,
			c.Name, c.Color, c.CreatorID, now))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO calendar_members (calendar_id, user_id, joined_at) VALUES (?, ?, ?)`,
			out.ID, c.CreatorID, now); err != nil {
			return mapErr(err)
		}
		for i, l := range defaultLabels {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO labels (calendar_id, name, color, position) VALUES (?, ?, ?, ?)`,
				out.ID, l.Name, l.Color, i); err != nil {
				return mapErr(err)
			}
		}
		return nil
	})
	if err != nil {
		return domain.Calendar{}, fmt.Errorf("create calendar %q: %w", c.Name, err)
	}
	return out, nil
}

// CalendarByID returns the calendar, or domain.ErrNotFound.
func (s *Store) CalendarByID(ctx context.Context, id int64) (domain.Calendar, error) {
	c, err := scanCalendar(s.q.QueryRowContext(ctx, `SELECT `+calendarColsBare+` FROM calendars WHERE id = ?`, id))
	if err != nil {
		return domain.Calendar{}, fmt.Errorf("calendar %d: %w", id, err)
	}
	return c, nil
}

// UpdateCalendar saves the name and colour. Ownership moves only through
// TransferCreator, so a stale Calendar value round-tripped through an HTTP handler
// cannot silently hand the calendar to somebody else.
func (s *Store) UpdateCalendar(ctx context.Context, c domain.Calendar) error {
	err := affected(s.q.ExecContext(ctx,
		`UPDATE calendars SET name = ?, color = ? WHERE id = ?`, c.Name, c.Color, c.ID))
	if err != nil {
		return fmt.Errorf("update calendar %d: %w", c.ID, err)
	}
	return nil
}

// DeleteCalendar removes a calendar and, by ON DELETE CASCADE, its members, labels,
// invites and events — and through those, participants, overrides and reminders.
//
// Two things the cascade cannot reach have to go first, in the same transaction, while
// the events that name them are still there to name them:
//
// Recurrences have no calendar_id, and events.recurrence_id is ON DELETE SET NULL — the
// direction that lets a pattern be dropped from a series without taking the event with
// it — so deleting the events lets go of the recurrences rather than following them.
// What is left is a pattern belonging to nothing, and the reminders hanging off it,
// which the planner walks on every pass. A recurrences.calendar_id column would make
// this a cascade, but not without a nullable column, a backfill and a signature change
// in three places, since SQLite cannot add a NOT NULL column with no default in place —
// only rebuild the table, which is the opposite of the expand-only rule migrations here
// follow (CONVENTIONS §8).
//
// Queued notifications are the half a family actually sees. The outbox is denormalised
// on purpose, so that it survives the thing it announces being edited, and delivery
// never re-checks that the event is still there: a reminder already materialised for
// the next two days goes out for a calendar that no longer exists. Only undelivered
// rows go; sent and skipped ones are history.
func (s *Store) DeleteCalendar(ctx context.Context, id int64) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM recurrences
			 WHERE id IN (SELECT recurrence_id FROM events
			               WHERE calendar_id = ? AND recurrence_id IS NOT NULL)`, id); err != nil {
			return mapErr(err)
		}
		// source_ref is "reminder:{eventID}:{occurrenceDate}:{reminderID}" — the layout
		// internal/events.ReminderSourceRef writes and prunes by, which cannot be
		// imported here (it depends on this package). TestDeletingACalendarPrunesTheOutbox
		// in internal/events holds the two together.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM notification_queue
			 WHERE sent_at IS NULL AND skipped IS NULL
			   AND EXISTS (SELECT 1 FROM events e
			                WHERE e.calendar_id = ?
			                  AND notification_queue.source_ref LIKE 'reminder:' || e.id || ':%')`, id); err != nil {
			return mapErr(err)
		}
		return affected(tx.ExecContext(ctx, `DELETE FROM calendars WHERE id = ?`, id))
	})
	if err != nil {
		return fmt.Errorf("delete calendar %d: %w", id, err)
	}
	return nil
}

// ListCalendarsForUser returns the calendars a user is a member of, ordered by name.
// It is the authorisation source of truth for "which calendars may this request see".
func (s *Store) ListCalendarsForUser(ctx context.Context, userID int64) ([]domain.Calendar, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+calendarColsC+`
		  FROM calendars c
		  JOIN calendar_members m ON m.calendar_id = c.id
		 WHERE m.user_id = ?
		 ORDER BY c.name, c.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list calendars for user %d: %w", userID, mapErr(err))
	}
	defer rows.Close()
	var out []domain.Calendar
	for rows.Next() {
		c, err := scanCalendar(rows)
		if err != nil {
			return nil, fmt.Errorf("list calendars for user %d: %w", userID, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list calendars for user %d: %w", userID, err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Membership
// ---------------------------------------------------------------------------

const memberCols = `calendar_id, user_id, muted, participating_only, joined_at`

func scanMember(row rowScanner) (domain.Member, error) {
	var m domain.Member
	err := row.Scan(&m.CalendarID, &m.UserID, &m.Muted, &m.ParticipatingOnly, instantCol{&m.JoinedAt})
	if err != nil {
		return domain.Member{}, mapErr(err)
	}
	return m, nil
}

// IsMember is the per-request authorisation check: a user may only touch calendars
// they belong to.
func (s *Store) IsMember(ctx context.Context, calendarID, userID int64) (bool, error) {
	var one int
	err := s.q.QueryRowContext(ctx,
		`SELECT 1 FROM calendar_members WHERE calendar_id = ? AND user_id = ?`, calendarID, userID).Scan(&one)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("membership of calendar %d by user %d: %w", calendarID, userID, mapErr(err))
	}
	return true, nil
}

// AddMember joins a user to a calendar with the default notification settings.
//
// It is idempotent, because the invite link that leads here is multi-use and a second
// click must not be an error. Callers that log a member_joined activity should check
// IsMember first, so a re-click does not log twice.
func (s *Store) AddMember(ctx context.Context, calendarID, userID int64) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO calendar_members (calendar_id, user_id, joined_at) VALUES (?, ?, ?)
		ON CONFLICT (calendar_id, user_id) DO NOTHING`,
		calendarID, userID, mustInstant(s.now()))
	if err != nil {
		return fmt.Errorf("add user %d to calendar %d: %w", userID, calendarID, mapErr(err))
	}
	return nil
}

// RemoveMember removes a membership and everything in that calendar that was only
// there because of it, reporting domain.ErrNotFound when there was no membership.
//
// event_participants and reminders are keyed on users rather than on membership, so the
// membership row is not the only thing that has to go. What is left otherwise is an
// ex-member still shown on other people's events — a state the API refuses to create,
// since an edit that lists a non-member is rejected — and their reminders lying dormant
// until somebody re-invites them, at which point they start firing again. Both are
// scoped to this calendar: the same person's rows in the calendars they are staying in
// are none of this method's business. The reminders reach the series patterns by joining
// through events, which is the same route DeleteCalendar takes.
//
// The membership row goes last, so its domain.ErrNotFound rolls the rest back: removing
// somebody who was never there stays an error that changed nothing.
//
// It does not touch the calendar's creator_id: removing the creator leaves a calendar
// with a dangling owner, so the caller must pair this with TransferCreator (or
// DeleteCalendar when nobody is left). Only the creator is allowed to remove other
// people — that check belongs in the handler, not here.
func (s *Store) RemoveMember(ctx context.Context, calendarID, userID int64) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM event_participants
			 WHERE user_id = ?
			   AND event_id IN (SELECT id FROM events WHERE calendar_id = ?)`,
			userID, calendarID); err != nil {
			return mapErr(err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM reminders
			 WHERE user_id = ?
			   AND (event_id IN (SELECT id FROM events WHERE calendar_id = ?)
			     OR recurrence_id IN (SELECT recurrence_id FROM events
			                           WHERE calendar_id = ? AND recurrence_id IS NOT NULL))`,
			userID, calendarID, calendarID); err != nil {
			return mapErr(err)
		}
		return affected(tx.ExecContext(ctx,
			`DELETE FROM calendar_members WHERE calendar_id = ? AND user_id = ?`, calendarID, userID))
	})
	if err != nil {
		return fmt.Errorf("remove user %d from calendar %d: %w", userID, calendarID, err)
	}
	return nil
}

// ListMembers returns a calendar's members, oldest first — the same order
// TransferCreator promotes in.
func (s *Store) ListMembers(ctx context.Context, calendarID int64) ([]domain.Member, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+memberCols+` FROM calendar_members WHERE calendar_id = ? ORDER BY joined_at, user_id`, calendarID)
	if err != nil {
		return nil, fmt.Errorf("list members of calendar %d: %w", calendarID, mapErr(err))
	}
	defer rows.Close()
	var out []domain.Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, fmt.Errorf("list members of calendar %d: %w", calendarID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list members of calendar %d: %w", calendarID, err)
	}
	return out, nil
}

// Membership returns one (calendar, user) pair, including its mute and
// participating-only flags, or domain.ErrNotFound.
func (s *Store) Membership(ctx context.Context, calendarID, userID int64) (domain.Member, error) {
	m, err := scanMember(s.q.QueryRowContext(ctx,
		`SELECT `+memberCols+` FROM calendar_members WHERE calendar_id = ? AND user_id = ?`, calendarID, userID))
	if err != nil {
		return domain.Member{}, fmt.Errorf("membership of calendar %d by user %d: %w", calendarID, userID, err)
	}
	return m, nil
}

// UpdateMembership saves the per-pair notification settings. JoinedAt is immutable.
func (s *Store) UpdateMembership(ctx context.Context, m domain.Member) error {
	err := affected(s.q.ExecContext(ctx, `
		UPDATE calendar_members SET muted = ?, participating_only = ?
		 WHERE calendar_id = ? AND user_id = ?`,
		boolArg(m.Muted), boolArg(m.ParticipatingOnly), m.CalendarID, m.UserID))
	if err != nil {
		return fmt.Errorf("update membership of calendar %d by user %d: %w", m.CalendarID, m.UserID, err)
	}
	return nil
}

// CountMembers returns how many people are in a calendar.
func (s *Store) CountMembers(ctx context.Context, calendarID int64) (int, error) {
	var n int
	err := s.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM calendar_members WHERE calendar_id = ?`, calendarID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count members of calendar %d: %w", calendarID, mapErr(err))
	}
	return n, nil
}

// TransferCreator hands a calendar to its longest-standing member other than the
// current creator, so that the creator can leave without orphaning it.
//
// It returns domain.ErrNotFound when the calendar does not exist or has no other
// member — in the latter case the caller's only sensible move is DeleteCalendar.
func (s *Store) TransferCreator(ctx context.Context, calendarID int64) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var creatorID int64
		if err := tx.QueryRowContext(ctx, `SELECT creator_id FROM calendars WHERE id = ?`, calendarID).Scan(&creatorID); err != nil {
			return mapErr(err)
		}
		var next int64
		err := tx.QueryRowContext(ctx, `
			SELECT user_id FROM calendar_members
			 WHERE calendar_id = ? AND user_id <> ?
			 ORDER BY joined_at, user_id
			 LIMIT 1`, calendarID, creatorID).Scan(&next)
		if err != nil {
			if isNotFound(err) {
				return fmt.Errorf("no other member to promote: %w", domain.ErrNotFound)
			}
			return mapErr(err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE calendars SET creator_id = ? WHERE id = ?`, next, calendarID)
		return mapErr(err)
	})
	if err != nil {
		return fmt.Errorf("transfer creator of calendar %d: %w", calendarID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

const labelCols = `id, calendar_id, name, color, position`

func scanLabel(row rowScanner) (domain.Label, error) {
	var l domain.Label
	if err := row.Scan(&l.ID, &l.CalendarID, &l.Name, &l.Color, &l.Position); err != nil {
		return domain.Label{}, mapErr(err)
	}
	return l, nil
}

// ListLabels returns a calendar's ten labels in display order.
func (s *Store) ListLabels(ctx context.Context, calendarID int64) ([]domain.Label, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+labelCols+` FROM labels WHERE calendar_id = ? ORDER BY position, id`, calendarID)
	if err != nil {
		return nil, fmt.Errorf("list labels of calendar %d: %w", calendarID, mapErr(err))
	}
	defer rows.Close()
	var out []domain.Label
	for rows.Next() {
		l, err := scanLabel(rows)
		if err != nil {
			return nil, fmt.Errorf("list labels of calendar %d: %w", calendarID, err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list labels of calendar %d: %w", calendarID, err)
	}
	return out, nil
}

// LabelByID returns one label, or domain.ErrNotFound. Handlers use it to check that a
// label belongs to the calendar the event is being filed under.
func (s *Store) LabelByID(ctx context.Context, id int64) (domain.Label, error) {
	l, err := scanLabel(s.q.QueryRowContext(ctx, `SELECT `+labelCols+` FROM labels WHERE id = ?`, id))
	if err != nil {
		return domain.Label{}, fmt.Errorf("label %d: %w", id, err)
	}
	return l, nil
}

// UpdateLabel renames, recolours or reorders a label. There is deliberately no
// CreateLabel or DeleteLabel: the ten seeded rows are the whole set, forever.
func (s *Store) UpdateLabel(ctx context.Context, l domain.Label) error {
	err := affected(s.q.ExecContext(ctx,
		`UPDATE labels SET name = ?, color = ?, position = ? WHERE id = ?`, l.Name, l.Color, l.Position, l.ID))
	if err != nil {
		return fmt.Errorf("update label %d: %w", l.ID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Invites
// ---------------------------------------------------------------------------

const inviteColsBare = `id, calendar_id, created_by, created_at, expires_at, revoked_at, used_count`
const inviteColsI = `i.id, i.calendar_id, i.created_by, i.created_at, i.expires_at, i.revoked_at, i.used_count`

func scanInvite(row rowScanner, inv *domain.Invite) error {
	return row.Scan(&inv.ID, &inv.CalendarID, &inv.CreatedBy,
		instantCol{&inv.CreatedAt}, instantCol{&inv.ExpiresAt},
		instantPtrCol{&inv.RevokedAt}, &inv.UsedCount)
}

// CreateInvite records a join link. Only the SHA-256 of the token is stored; the
// plaintext lives in the URL that gets sent and nowhere else.
//
// The store assigns ID, CreatedAt and UsedCount. ExpiresAt is the caller's: a zero
// value falls back to now + domain.InviteTTL.
func (s *Store) CreateInvite(ctx context.Context, inv domain.Invite, tokenHash string) (domain.Invite, error) {
	now := s.now()
	if inv.ExpiresAt.IsZero() {
		inv.ExpiresAt = now.Add(domain.InviteTTL)
	}
	var out domain.Invite
	err := scanInvite(s.q.QueryRowContext(ctx, `
		INSERT INTO invites (token_hash, calendar_id, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
		RETURNING `+inviteColsBare,
		tokenHash, inv.CalendarID, inv.CreatedBy, mustInstant(now), mustInstant(inv.ExpiresAt),
	), &out)
	if err != nil {
		return domain.Invite{}, fmt.Errorf("create invite for calendar %d: %w", inv.CalendarID, mapErr(err))
	}
	return out, nil
}

// InviteByToken resolves an invite token to the invite and the calendar it opens.
//
// Revoked and expired invites are reported as domain.ErrNotFound, indistinguishably
// from an unknown token: a stale link tells its holder nothing about whether the
// calendar exists. now is a parameter rather than the store's clock so that the
// signup handler judges the invite against the same instant it used for everything
// else in the request.
func (s *Store) InviteByToken(ctx context.Context, tokenHash string, now time.Time) (domain.Invite, domain.Calendar, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT `+inviteColsI+`, `+calendarColsC+`
		  FROM invites i
		  JOIN calendars c ON c.id = i.calendar_id
		 WHERE i.token_hash = ? AND i.revoked_at IS NULL AND i.expires_at > ?`,
		tokenHash, mustInstant(now))

	var inv domain.Invite
	var c domain.Calendar
	err := row.Scan(
		&inv.ID, &inv.CalendarID, &inv.CreatedBy,
		instantCol{&inv.CreatedAt}, instantCol{&inv.ExpiresAt},
		instantPtrCol{&inv.RevokedAt}, &inv.UsedCount,
		&c.ID, &c.Name, &c.Color, &c.CreatorID, instantCol{&c.CreatedAt}, &c.HasImage,
	)
	if err != nil {
		return domain.Invite{}, domain.Calendar{}, fmt.Errorf("invite by token: %w", mapErr(err))
	}
	return inv, c, nil
}

// IncrementInviteUse counts one redemption. Invites are multi-use within their window
// (a parent sends one link to the whole household), so this is a counter for the
// settings screen, not a limit.
func (s *Store) IncrementInviteUse(ctx context.Context, id int64) error {
	err := affected(s.q.ExecContext(ctx, `UPDATE invites SET used_count = used_count + 1 WHERE id = ?`, id))
	if err != nil {
		return fmt.Errorf("increment invite %d: %w", id, err)
	}
	return nil
}

// ListInvites returns a calendar's invites, newest first, including revoked and
// expired ones — the settings screen shows them so a link can be recognised and
// revoked.
func (s *Store) ListInvites(ctx context.Context, calendarID int64) ([]domain.Invite, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+inviteColsBare+` FROM invites WHERE calendar_id = ? ORDER BY created_at DESC, id DESC`, calendarID)
	if err != nil {
		return nil, fmt.Errorf("list invites of calendar %d: %w", calendarID, mapErr(err))
	}
	defer rows.Close()
	var out []domain.Invite
	for rows.Next() {
		var inv domain.Invite
		if err := scanInvite(rows, &inv); err != nil {
			return nil, fmt.Errorf("list invites of calendar %d: %w", calendarID, mapErr(err))
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list invites of calendar %d: %w", calendarID, err)
	}
	return out, nil
}

// RevokeInvite kills a link. Revoking an already-revoked invite is domain.ErrNotFound,
// which keeps the revoke button honest about whether it did anything.
func (s *Store) RevokeInvite(ctx context.Context, id int64, now time.Time) error {
	err := affected(s.q.ExecContext(ctx,
		`UPDATE invites SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, mustInstant(now), id))
	if err != nil {
		return fmt.Errorf("revoke invite %d: %w", id, err)
	}
	return nil
}

// SetCalendarImage stores a calendar's cover image, or clears it when img is nil.
// The bytes are whatever internal/imgproc produced: a small square JPEG.
func (s *Store) SetCalendarImage(ctx context.Context, calendarID int64, img []byte) error {
	var arg any
	if img != nil {
		arg = img
	}
	if err := affected(s.q.ExecContext(ctx, `UPDATE calendars SET image = ? WHERE id = ?`, arg, calendarID)); err != nil {
		return fmt.Errorf("set image for calendar %d: %w", calendarID, err)
	}
	return nil
}

// CalendarImage returns the stored cover image, or domain.ErrNotFound when the
// calendar has none.
func (s *Store) CalendarImage(ctx context.Context, calendarID int64) ([]byte, error) {
	var img []byte
	err := s.q.QueryRowContext(ctx, `SELECT image FROM calendars WHERE id = ?`, calendarID).Scan(&img)
	if err != nil {
		return nil, fmt.Errorf("calendar %d image: %w", calendarID, mapErr(err))
	}
	if img == nil {
		return nil, fmt.Errorf("calendar %d has no image: %w", calendarID, domain.ErrNotFound)
	}
	return img, nil
}
