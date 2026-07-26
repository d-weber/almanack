package store

import (
	"context"
	"fmt"
	"time"

	"agenda/internal/domain"
)

// users.avatar is a BLOB and is never selected alongside the rest of a user: an avatar
// is tens of kilobytes and every list would carry it. Only its presence is projected,
// as User.HasAvatar; Avatar fetches the bytes.
const (
	userColsBare = `id, email, display_name, color, lang, week_start, time_format, avatar IS NOT NULL, is_admin, created_at`
	userColsU    = `u.id, u.email, u.display_name, u.color, u.lang, u.week_start, u.time_format, u.avatar IS NOT NULL, u.is_admin, u.created_at`
)

func scanUser(row rowScanner) (domain.User, error) {
	var u domain.User
	var weekday int
	err := row.Scan(
		&u.ID, &u.Email, &u.DisplayName, &u.Color, &u.Lang, &weekday,
		&u.TimeFormat, &u.HasAvatar, &u.IsAdmin, instantCol{&u.CreatedAt},
	)
	if err != nil {
		return domain.User{}, mapErr(err)
	}
	u.WeekStart = time.Weekday(weekday)
	return u, nil
}

// CreateUser inserts a user with an already-hashed password. Hashing is the caller's
// job: the store never sees a plaintext password, and argon2id parameters are a policy
// decision that does not belong next to the SQL.
//
// The store assigns ID and CreatedAt; those fields of u are ignored. Empty Lang and
// TimeFormat fall back to the schema defaults rather than tripping their CHECK.
// A duplicate email (compared case-insensitively) is domain.ErrConflict.
func (s *Store) CreateUser(ctx context.Context, u domain.User, passwordHash string) (domain.User, error) {
	if u.Lang == "" {
		u.Lang = domain.LangFR
	}
	if u.TimeFormat == "" {
		u.TimeFormat = "24h"
	}
	created, err := scanUser(s.db.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash, display_name, color, lang, week_start, time_format, is_admin, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING `+userColsBare,
		u.Email, passwordHash, u.DisplayName, u.Color, string(u.Lang),
		int(u.WeekStart), u.TimeFormat, boolArg(u.IsAdmin), mustInstant(s.now()),
	))
	if err != nil {
		return domain.User{}, fmt.Errorf("create user %q: %w", u.Email, err)
	}
	return created, nil
}

// UserByID returns the user, or domain.ErrNotFound.
func (s *Store) UserByID(ctx context.Context, id int64) (domain.User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColsBare+` FROM users WHERE id = ?`, id))
	if err != nil {
		return domain.User{}, fmt.Errorf("user %d: %w", id, err)
	}
	return u, nil
}

// UserByEmail looks a user up by email, case-insensitively (users.email is
// COLLATE NOCASE), or returns domain.ErrNotFound.
//
// Callers on the login and password-reset paths must not let the difference between
// this returning a user and returning ErrNotFound reach the client: the HTTP response
// is identical either way, or the endpoint becomes an account-enumeration oracle.
func (s *Store) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColsBare+` FROM users WHERE email = ?`, email))
	if err != nil {
		return domain.User{}, fmt.Errorf("user %q: %w", email, err)
	}
	return u, nil
}

// UserPasswordHash returns the stored argon2id hash for a user. It is separate from
// UserByID so that the hash is fetched only where it is actually verified, and cannot
// ride along into a JSON response by accident.
func (s *Store) UserPasswordHash(ctx context.Context, id int64) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, id).Scan(&hash)
	if err != nil {
		return "", fmt.Errorf("password hash for user %d: %w", id, mapErr(err))
	}
	return hash, nil
}

// UpdateUser saves the editable profile fields: email, display name, color, language,
// week start, time format and the admin flag. Password and avatar have their own
// setters. A duplicate email is domain.ErrConflict.
func (s *Store) UpdateUser(ctx context.Context, u domain.User) error {
	err := affected(s.db.ExecContext(ctx, `
		UPDATE users
		   SET email = ?, display_name = ?, color = ?, lang = ?, week_start = ?, time_format = ?, is_admin = ?
		 WHERE id = ?`,
		u.Email, u.DisplayName, u.Color, string(u.Lang), int(u.WeekStart),
		u.TimeFormat, boolArg(u.IsAdmin), u.ID,
	))
	if err != nil {
		return fmt.Errorf("update user %d: %w", u.ID, err)
	}
	return nil
}

// SetPassword replaces a user's password hash.
//
// Every caller must also call DeleteUserSessions: a password change exists to lock
// somebody out, and leaving their sessions alive means it did not.
func (s *Store) SetPassword(ctx context.Context, userID int64, passwordHash string) error {
	err := affected(s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, userID))
	if err != nil {
		return fmt.Errorf("set password for user %d: %w", userID, err)
	}
	return nil
}

// SetAvatar stores an already-decoded, already-scaled avatar blob (internal/imgproc
// does that work). Passing nil is the same as DeleteAvatar.
func (s *Store) SetAvatar(ctx context.Context, userID int64, img []byte) error {
	var arg any
	if img != nil {
		arg = img
	}
	err := affected(s.db.ExecContext(ctx, `UPDATE users SET avatar = ? WHERE id = ?`, arg, userID))
	if err != nil {
		return fmt.Errorf("set avatar for user %d: %w", userID, err)
	}
	return nil
}

// Avatar returns the stored avatar bytes. It reports domain.ErrNotFound both when the
// user does not exist and when they have no avatar, which is the same 404 either way.
func (s *Store) Avatar(ctx context.Context, userID int64) ([]byte, error) {
	var img []byte
	err := s.db.QueryRowContext(ctx, `SELECT avatar FROM users WHERE id = ?`, userID).Scan(&img)
	if err != nil {
		return nil, fmt.Errorf("avatar for user %d: %w", userID, mapErr(err))
	}
	if img == nil {
		return nil, fmt.Errorf("avatar for user %d: %w", userID, domain.ErrNotFound)
	}
	return img, nil
}

// DeleteAvatar clears a user's avatar.
func (s *Store) DeleteAvatar(ctx context.Context, userID int64) error {
	err := affected(s.db.ExecContext(ctx, `UPDATE users SET avatar = NULL WHERE id = ?`, userID))
	if err != nil {
		return fmt.Errorf("delete avatar for user %d: %w", userID, err)
	}
	return nil
}

// ListUsers returns every account, ordered by display name. A family deployment has at
// most a couple of dozen, so this is deliberately unpaginated.
func (s *Store) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColsBare+` FROM users ORDER BY display_name, id`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", mapErr(err))
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("list users: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return out, nil
}

// CountUsers returns the number of accounts. Zero means first-run: the setup flow uses
// it to decide whether the next account created becomes the admin.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", mapErr(err))
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

const sessionCols = `s.id, s.user_id, s.created_at, s.last_seen_at, s.expires_at`

func scanSession(row rowScanner, s *domain.Session) error {
	return row.Scan(&s.ID, &s.UserID, instantCol{&s.CreatedAt}, instantCol{&s.LastSeenAt}, instantCol{&s.ExpiresAt})
}

// CreateSession records a logged-in browser. tokenHash is the SHA-256 of the cookie
// value: the plaintext token exists only in the cookie, so a database leak does not
// hand over live sessions.
func (s *Store) CreateSession(ctx context.Context, userID int64, tokenHash string, expires time.Time) (domain.Session, error) {
	now := s.now()
	var sess domain.Session
	err := scanSession(s.db.QueryRowContext(ctx, `
		INSERT INTO sessions (token_hash, user_id, created_at, last_seen_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
		RETURNING id, user_id, created_at, last_seen_at, expires_at`,
		tokenHash, userID, mustInstant(now), mustInstant(now), mustInstant(expires),
	), &sess)
	if err != nil {
		return domain.Session{}, fmt.Errorf("create session for user %d: %w", userID, mapErr(err))
	}
	return sess, nil
}

// SessionByToken resolves a session cookie to its session and its user in one query.
//
// Expired sessions are treated as absent and reported as domain.ErrNotFound: the store
// filters on its own clock rather than trusting every caller to remember the check.
// Sliding renewal is TouchSession's job.
func (s *Store) SessionByToken(ctx context.Context, tokenHash string) (domain.Session, domain.User, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+sessionCols+`, `+userColsU+`
		  FROM sessions s
		  JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = ? AND s.expires_at > ?`,
		tokenHash, mustInstant(s.now()))

	var sess domain.Session
	var u domain.User
	var weekday int
	err := row.Scan(
		&sess.ID, &sess.UserID, instantCol{&sess.CreatedAt}, instantCol{&sess.LastSeenAt}, instantCol{&sess.ExpiresAt},
		&u.ID, &u.Email, &u.DisplayName, &u.Color, &u.Lang, &weekday,
		&u.TimeFormat, &u.HasAvatar, &u.IsAdmin, instantCol{&u.CreatedAt},
	)
	if err != nil {
		return domain.Session{}, domain.User{}, fmt.Errorf("session by token: %w", mapErr(err))
	}
	u.WeekStart = time.Weekday(weekday)
	return sess, u, nil
}

// TouchSession slides the session window on use, which is what makes the 90-day
// expiry a rolling one rather than a hard logout every quarter.
func (s *Store) TouchSession(ctx context.Context, id int64, lastSeen, expires time.Time) error {
	err := affected(s.db.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`,
		mustInstant(lastSeen), mustInstant(expires), id))
	if err != nil {
		return fmt.Errorf("touch session %d: %w", id, err)
	}
	return nil
}

// DeleteSession logs one browser out. It is idempotent: logging out of a session that
// has already expired or been deleted is a success, not a 404.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("delete session: %w", mapErr(err))
	}
	return nil
}

// DeleteUserSessions logs a user out everywhere. Idempotent. Every password change and
// every completed password reset must call it.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("delete sessions for user %d: %w", userID, mapErr(err))
	}
	return nil
}

// DeleteExpiredSessions prunes sessions that expired before now and returns how many
// went. The scheduler calls it; nothing depends on it having run, since SessionByToken
// already ignores expired rows.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, mustInstant(now))
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return int(n), nil
}

// ---------------------------------------------------------------------------
// Password resets
// ---------------------------------------------------------------------------

// CreatePasswordReset records a password-reset token. As with sessions, only the
// SHA-256 is stored; the plaintext goes out in the email and nowhere else.
func (s *Store) CreatePasswordReset(ctx context.Context, userID int64, tokenHash string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO password_resets (token_hash, user_id, created_at, expires_at)
		VALUES (?, ?, ?, ?)`,
		tokenHash, userID, mustInstant(s.now()), mustInstant(expires))
	if err != nil {
		return fmt.Errorf("create password reset for user %d: %w", userID, mapErr(err))
	}
	return nil
}

// ConsumePasswordReset redeems a reset token and returns the user it belongs to.
//
// The whole check-and-burn is one UPDATE ... RETURNING, so two concurrent redemptions
// of the same token cannot both succeed: the second matches no row because used_at is
// no longer NULL. An expired, already-used or unknown token is domain.ErrNotFound —
// the three are indistinguishable to the caller by design.
func (s *Store) ConsumePasswordReset(ctx context.Context, tokenHash string, now time.Time) (int64, error) {
	var userID int64
	err := s.db.QueryRowContext(ctx, `
		UPDATE password_resets
		   SET used_at = ?
		 WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
		RETURNING user_id`,
		mustInstant(now), tokenHash, mustInstant(now),
	).Scan(&userID)
	if err != nil {
		return 0, fmt.Errorf("consume password reset: %w", mapErr(err))
	}
	return userID, nil
}
