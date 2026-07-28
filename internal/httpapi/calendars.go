package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"almanack/internal/auth"
	"almanack/internal/domain"
	"almanack/internal/imgproc"
	"almanack/internal/store"
)

type calendarRequest struct {
	Name  *string `json:"name"`
	Color *string `json:"color"`
}

func (s *Server) handleCreateCalendar(w http.ResponseWriter, r *http.Request) {
	var req calendarRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	if req.Name == nil || req.Color == nil {
		fail(w, r, invalidf("a calendar needs a name and a colour"))
		return
	}
	name, err := cleanText(*req.Name, maxNameLen, "the calendar name")
	if err != nil {
		fail(w, r, err)
		return
	}
	if name == "" {
		fail(w, r, invalidf("a calendar needs a name"))
		return
	}
	color, err := normalizeColor(*req.Color)
	if err != nil {
		fail(w, r, err)
		return
	}

	ctx := r.Context()
	user := userOf(ctx)
	// The store seeds the creator's membership and the ten labels in the same
	// transaction, so a calendar is never briefly unusable.
	cal, err := s.store.CreateCalendar(ctx, domain.Calendar{Name: name, Color: color, CreatorID: user.ID})
	if err != nil {
		fail(w, r, err)
		return
	}
	view, err := s.calendarView(ctx, cal, user.ID, nil)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, view)
}

func (s *Server) handlePatchCalendar(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	var req calendarRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	ctx := r.Context()
	user := userOf(ctx)
	if err := s.requireMember(ctx, id, user.ID); err != nil {
		fail(w, r, err)
		return
	}
	cal, err := s.store.CalendarByID(ctx, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	if req.Name != nil {
		name, err := cleanText(*req.Name, maxNameLen, "the calendar name")
		if err != nil {
			fail(w, r, err)
			return
		}
		if name == "" {
			fail(w, r, invalidf("a calendar needs a name"))
			return
		}
		cal.Name = name
	}
	if req.Color != nil {
		color, err := normalizeColor(*req.Color)
		if err != nil {
			fail(w, r, err)
			return
		}
		cal.Color = color
	}
	if err := s.store.UpdateCalendar(ctx, cal); err != nil {
		fail(w, r, err)
		return
	}
	view, err := s.calendarView(ctx, cal, user.ID, nil)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, view)
}

// handleDeleteCalendar refuses while anyone else is still in the calendar: deleting it
// would take their events with it, and one member's cleanup is not another's.
func (s *Server) handleDeleteCalendar(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	ctx := r.Context()
	user := userOf(ctx)
	if err := s.requireMember(ctx, id, user.ID); err != nil {
		fail(w, r, err)
		return
	}
	count, err := s.store.CountMembers(ctx, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	if count > 1 {
		fail(w, r, fmt.Errorf("%w: other members are still in this calendar; leave it instead", domain.ErrConflict))
		return
	}
	if err := s.store.DeleteCalendar(ctx, id); err != nil {
		fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// handleLeaveCalendar removes the caller. The creator role passes to the
// longest-standing remaining member, so leaving never orphans a calendar; the last
// member out deletes it.
//
// Counting the members and acting on the count are one transaction, because they are one
// decision. Two people leaving a two-member calendar at the same moment each read a count
// of two, each took the "somebody is still here" branch, and both memberships went: a
// calendar with no members at all, which no query returns to anybody and which nothing
// left in the application can reach — its events included. The DSN takes the write lock
// at BEGIN (`_txlock=immediate`), so the second request waits and then reads the count
// the first one left behind, which is one, and deletes the calendar as the last member
// out should.
func (s *Server) handleLeaveCalendar(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	ctx := r.Context()
	user := userOf(ctx)
	if err := s.requireMember(ctx, id, user.ID); err != nil {
		fail(w, r, err)
		return
	}

	left := false
	err = s.store.InTx(ctx, func(st *store.Store) error {
		cal, err := st.CalendarByID(ctx, id)
		if err != nil {
			return err
		}
		count, err := st.CountMembers(ctx, id)
		if err != nil {
			return err
		}
		if count <= 1 {
			return st.DeleteCalendar(ctx, id)
		}
		if cal.CreatorID == user.ID {
			if err := st.TransferCreator(ctx, id); err != nil {
				return err
			}
		}
		if err := st.RemoveMember(ctx, id, user.ID); err != nil {
			return err
		}
		left = true
		return nil
	})
	if err != nil {
		fail(w, r, err)
		return
	}

	// Outside the transaction: a feed row that will not insert must not undo the
	// departure it describes, which is the same trade internal/events makes the other
	// way for an edit — there the row is what a change notification is planned from,
	// and here there is nothing to plan.
	if left {
		if err := s.store.LogActivity(ctx, domain.Activity{
			CalendarID: id,
			UserID:     user.ID,
			Action:     domain.ActionMemberLeft,
			Title:      user.DisplayName,
		}); err != nil {
			slog.Error("record activity", "action", domain.ActionMemberLeft, "error", err)
		}
	}
	writeNoContent(w)
}

type membershipRequest struct {
	Muted             *bool `json:"muted"`
	ParticipatingOnly *bool `json:"participating_only"`
}

func (s *Server) handlePatchMembership(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	var req membershipRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	ctx := r.Context()
	user := userOf(ctx)
	// The caller may only edit their own membership; there is no path to anyone else's.
	m, err := s.store.Membership(ctx, id, user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	if req.Muted != nil {
		m.Muted = *req.Muted
	}
	if req.ParticipatingOnly != nil {
		m.ParticipatingOnly = *req.ParticipatingOnly
	}
	if err := s.store.UpdateMembership(ctx, m); err != nil {
		fail(w, r, err)
		return
	}
	cal, err := s.store.CalendarByID(ctx, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	view, err := s.calendarView(ctx, cal, user.ID, nil)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, view)
}

// handleRemoveMember is the one asymmetric permission in the application: only the
// creator may remove somebody else.
func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	target, err := pathInt64(r, "user_id")
	if err != nil {
		fail(w, r, err)
		return
	}

	ctx := r.Context()
	user := userOf(ctx)
	cal, err := s.store.CalendarByID(ctx, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	if cal.CreatorID != user.ID {
		fail(w, r, fmt.Errorf("%w: only the calendar's creator may remove members", domain.ErrForbidden))
		return
	}
	if target == cal.CreatorID {
		fail(w, r, fmt.Errorf("%w: the creator cannot be removed; leave the calendar instead", domain.ErrConflict))
		return
	}
	if err := s.store.RemoveMember(ctx, id, target); err != nil {
		fail(w, r, err)
		return
	}
	writeNoContent(w)
}

type labelRequest struct {
	Name     *string `json:"name"`
	Color    *string `json:"color"`
	Position *int    `json:"position"`
}

// handlePatchLabel renames, recolours or reorders one of the ten seeded labels. Labels
// are never created or deleted, which is what keeps every event's label_id valid
// forever.
func (s *Server) handlePatchLabel(w http.ResponseWriter, r *http.Request) {
	calendarID, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	labelID, err := pathInt64(r, "label_id")
	if err != nil {
		fail(w, r, err)
		return
	}
	var req labelRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	ctx := r.Context()
	user := userOf(ctx)
	if err := s.requireMember(ctx, calendarID, user.ID); err != nil {
		fail(w, r, err)
		return
	}
	label, err := s.store.LabelByID(ctx, labelID)
	if err != nil {
		fail(w, r, err)
		return
	}
	if label.CalendarID != calendarID {
		fail(w, r, fmt.Errorf("label %d is not in calendar %d: %w", labelID, calendarID, domain.ErrNotFound))
		return
	}
	if req.Name != nil {
		name, err := cleanText(*req.Name, maxNameLen, "the label name")
		if err != nil {
			fail(w, r, err)
			return
		}
		if name == "" {
			fail(w, r, invalidf("a label needs a name"))
			return
		}
		label.Name = name
	}
	if req.Color != nil {
		color, err := normalizeColor(*req.Color)
		if err != nil {
			fail(w, r, err)
			return
		}
		label.Color = color
	}
	if req.Position != nil {
		if *req.Position < 0 || *req.Position >= domain.LabelsPerCalendar {
			fail(w, r, invalidf("position must be between 0 and %d", domain.LabelsPerCalendar-1))
			return
		}
		label.Position = *req.Position
	}
	if err := s.store.UpdateLabel(ctx, label); err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, labelView{
		ID: label.ID, Name: label.Name, Color: label.Color, Position: label.Position,
	})
}

type inviteResponse struct {
	ID        int64     `json:"id"`
	Token     string    `json:"token"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// handleCreateInvite mints a join link. The token is returned once and never stored in
// clear: what the database holds is its SHA-256.
func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	ctx := r.Context()
	user := userOf(ctx)
	if err := s.requireMember(ctx, id, user.ID); err != nil {
		fail(w, r, err)
		return
	}

	token, hash, err := auth.NewToken()
	if err != nil {
		fail(w, r, err)
		return
	}
	invite, err := s.store.CreateInvite(ctx, domain.Invite{
		CalendarID: id,
		CreatedBy:  user.ID,
		ExpiresAt:  s.clock.Now().Add(domain.InviteTTL),
	}, hash)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, inviteResponse{
		ID:    invite.ID,
		Token: token,
		// The hash form, because the browser app is hash-routed: a path-only invite
		// link serves the shell with an empty hash, which lands the invitee on the
		// login screen — and signup is invite-only, so that is the end of the road
		// for them. The path form still works via the redirect in web/js/app.js, for
		// links sent before this was fixed.
		URL:       strings.TrimRight(s.cfg.BaseURL, "/") + "/#/join/" + token,
		ExpiresAt: invite.ExpiresAt,
	})
}

// handleListInvites returns the links that still work. Tokens are not included — the
// server could not produce them if it wanted to.
func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	ctx := r.Context()
	user := userOf(ctx)
	if err := s.requireMember(ctx, id, user.ID); err != nil {
		fail(w, r, err)
		return
	}
	invites, err := s.store.ListInvites(ctx, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	now := s.clock.Now()
	active := make([]domain.Invite, 0, len(invites))
	for _, inv := range invites {
		if inv.RevokedAt == nil && inv.ExpiresAt.After(now) {
			active = append(active, inv)
		}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"invites": active})
}

// handleRevokeInvite kills a link. The invite is looked up through the caller's own
// calendars, which is both the lookup and the authorization: an id belonging to a
// calendar they are not in simply does not exist for them.
func (s *Server) handleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	ctx := r.Context()
	user := userOf(ctx)

	cals, err := s.store.ListCalendarsForUser(ctx, user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	found := false
	for _, c := range cals {
		invites, err := s.store.ListInvites(ctx, c.ID)
		if err != nil {
			fail(w, r, err)
			return
		}
		for _, inv := range invites {
			if inv.ID == id {
				found = true
			}
		}
		if found {
			break
		}
	}
	if !found {
		fail(w, r, fmt.Errorf("invite %d: %w", id, domain.ErrNotFound))
		return
	}
	if err := s.store.RevokeInvite(ctx, id, s.clock.Now()); err != nil {
		fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// Cover images. Any member may set one: this is a shared calendar, and the family's
// permissions are deliberately flat everywhere else too.

func (s *Server) handlePutCalendarImage(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	ctx := r.Context()
	if err := s.requireMember(ctx, id, userOf(ctx).ID); err != nil {
		fail(w, r, err)
		return
	}

	// One byte over the limit is enough to tell that it is over the limit.
	body := http.MaxBytesReader(w, r.Body, imgproc.MaxUploadBytes+1)
	data, err := io.ReadAll(body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			fail(w, r, invalidf("an image must be at most %d bytes", imgproc.MaxUploadBytes))
			return
		}
		fail(w, r, invalidf("could not read the uploaded image"))
		return
	}
	if len(data) > imgproc.MaxUploadBytes {
		fail(w, r, invalidf("an image must be at most %d bytes", imgproc.MaxUploadBytes))
		return
	}

	// imgproc's errors wrap domain.ErrInvalid, so a bad image maps to 400 for free.
	processed, err := imgproc.Process(data)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := s.store.SetCalendarImage(ctx, id, processed); err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"has_image": true})
}

func (s *Server) handleDeleteCalendarImage(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	ctx := r.Context()
	if err := s.requireMember(ctx, id, userOf(ctx).ID); err != nil {
		fail(w, r, err)
		return
	}
	if err := s.store.SetCalendarImage(ctx, id, nil); err != nil {
		fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// handleCalendarImage serves a stored cover. The bytes are ours — imgproc re-encoded
// them — but nosniff is set anyway, because serving a stored blob back is the one
// place worth being paranoid about.
func (s *Server) handleCalendarImage(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	ctx := r.Context()
	if err := s.requireMember(ctx, id, userOf(ctx).ID); err != nil {
		fail(w, r, err)
		return
	}
	data, err := s.store.CalendarImage(ctx, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`

	h := w.Header()
	h.Set("Content-Type", imgproc.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("ETag", etag)
	h.Set("Cache-Control", "private, max-age=300")
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if _, err := w.Write(data); err != nil {
		slog.Debug("write calendar image", "calendar", id, "error", err)
	}
}
