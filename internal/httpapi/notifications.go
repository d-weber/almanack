package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"almanack/internal/domain"
	"almanack/internal/events"
)

func (s *Server) handleGetPrefs(w http.ResponseWriter, r *http.Request) {
	prefs, err := s.prefsView(r.Context(), userOf(r.Context()).ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, prefs)
}

type prefsRequest struct {
	DigestEnabled    *bool   `json:"digest_enabled"`
	DigestTime       *string `json:"digest_time"`
	DigestOnEmpty    *bool   `json:"digest_on_empty"`
	DailySummaryMode *bool   `json:"daily_summary_mode"`
	SummaryTime      *string `json:"summary_time"`
	EmailReminders   *bool   `json:"email_reminders"`
	EmailDigest      *bool   `json:"email_digest"`
	ActivityPush     *bool   `json:"activity_push"`
}

func (s *Server) handlePatchPrefs(w http.ResponseWriter, r *http.Request) {
	var req prefsRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	ctx := r.Context()
	user := userOf(ctx)

	prefs, err := s.store.Prefs(ctx, user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	if req.DigestEnabled != nil {
		prefs.DigestEnabled = *req.DigestEnabled
	}
	if req.DigestTime != nil {
		value, err := validateHHMM(*req.DigestTime, "digest_time")
		if err != nil {
			fail(w, r, err)
			return
		}
		prefs.DigestTime = value
	}
	if req.DigestOnEmpty != nil {
		prefs.DigestOnEmpty = *req.DigestOnEmpty
	}
	if req.DailySummaryMode != nil {
		prefs.DailySummaryMode = *req.DailySummaryMode
	}
	if req.SummaryTime != nil {
		value, err := validateHHMM(*req.SummaryTime, "summary_time")
		if err != nil {
			fail(w, r, err)
			return
		}
		prefs.SummaryTime = value
	}
	if req.EmailReminders != nil {
		prefs.EmailReminders = *req.EmailReminders
	}
	if req.EmailDigest != nil {
		prefs.EmailDigest = *req.EmailDigest
	}
	if req.ActivityPush != nil {
		prefs.ActivityPush = *req.ActivityPush
	}
	if err := s.store.UpdatePrefs(ctx, prefs); err != nil {
		fail(w, r, err)
		return
	}
	view, err := s.prefsView(ctx, user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, view)
}

type pushSubscriptionRequest struct {
	Endpoint string `json:"endpoint"`
	P256DH   string `json:"p256dh"`
	Auth     string `json:"auth"`
	UALabel  string `json:"ua_label"`
}

// handlePushSubscribe stores a browser's push subscription. It is idempotent per
// endpoint, because the client re-registers on every app open as part of the liveness
// loop, and registering counts as a confirmation: a browser that has just handed over
// working keys is by definition alive.
func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	var req pushSubscriptionRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	endpoint, err := s.validPushEndpoint(req.Endpoint)
	if err != nil {
		fail(w, r, err)
		return
	}
	if req.P256DH == "" || req.Auth == "" {
		fail(w, r, invalidf("a push subscription needs its p256dh and auth keys"))
		return
	}
	label, err := cleanText(req.UALabel, maxNameLen, "the device label")
	if err != nil {
		fail(w, r, err)
		return
	}

	ctx := r.Context()
	user := userOf(ctx)
	if err := s.store.UpsertPushSubscription(ctx, domain.PushSubscription{
		UserID:   user.ID,
		Endpoint: endpoint,
		P256DH:   req.P256DH,
		Auth:     req.Auth,
		UALabel:  label,
	}); err != nil {
		fail(w, r, err)
		return
	}
	if err := s.store.ConfirmPushSubscription(ctx, endpoint, s.clock.Now()); err != nil {
		fail(w, r, err)
		return
	}
	slog.Info("push subscription registered", "user", user.ID, "service", endpointHost(endpoint))
	writeNoContent(w)
}

type pushEndpointRequest struct {
	Endpoint string `json:"endpoint"`
}

// handlePushConfirm is the liveness ping the client sends on every app open. It is the
// only signal that tells a live iOS subscription from a silently revoked one, since the
// push service keeps returning success for the dead ones.
func (s *Server) handlePushConfirm(w http.ResponseWriter, r *http.Request) {
	var req pushEndpointRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	endpoint, err := validEndpoint(req.Endpoint)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := s.store.ConfirmPushSubscription(r.Context(), endpoint, s.clock.Now()); err != nil {
		fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// handlePushUnsubscribe drops a subscription. It is idempotent: the client calls it on
// logout, and a delivery failure may already have pruned the same row.
func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var req pushEndpointRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	endpoint, err := validEndpoint(req.Endpoint)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := s.store.DeletePushSubscription(r.Context(), endpoint); err != nil {
		fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// handlePushTest sends a notification to the caller's own devices, which is how somebody
// checks that notifications work without waiting for one to be due.
//
// When the notifier can send directly it does. Otherwise the message is queued as an
// ordinary reminder due in a minute, so it travels the real pipeline — the same
// planner, the same push and email delivery, the same payload format — rather than a
// special path that could work while the real one is broken. On a laptop with no push
// service at all it still shows up at /dev/notifications.
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := userOf(ctx)

	if sender, ok := s.notifier.(TestSender); ok {
		sent, err := sender.SendTest(ctx, user.ID)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"sent": sent})
		return
	}

	subs, err := s.store.ListPushSubscriptions(ctx, user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	now := s.clock.Now()
	// The payload is the notification *data*, never its text: the wording is resolved
	// at delivery time from the recipient's language, exactly like every other row.
	payload, err := json.Marshal(map[string]any{
		"kind":        string(domain.KindReminder),
		"title":       s.catalog.T(user.Lang, "notify.test.title", nil),
		"event_start": now.Add(time.Minute).UTC().Format(time.RFC3339),
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := s.store.EnqueueNotification(ctx, domain.QueuedNotification{
		UserID:    user.ID,
		Kind:      domain.KindReminder,
		SourceRef: events.TestSourceRef(user.ID, now.UnixNano()),
		Payload:   string(payload),
		DueAt:     now,
	}); err != nil {
		fail(w, r, err)
		return
	}
	slog.Info("test notification queued", "user", user.ID, "devices", len(subs))
	writeJSON(w, r, http.StatusOK, map[string]any{"sent": len(subs)})
}

// validEndpoint checks that a push endpoint is an https URL. Endpoints are never logged
// in full: one is a capability to send that device notifications.
//
// It is what confirming and unsubscribing use. Neither dials anything — they look a row
// up by its endpoint — so both must keep working for a subscription whose host the
// allowlist would now refuse, or narrowing the list would strand the devices already
// registered with no way to tidy up after themselves.
func validEndpoint(raw string) (string, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		return "", invalidf("a push endpoint is required")
	}
	if len(endpoint) > 2000 {
		return "", invalidf("that push endpoint is too long")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", invalidf("a push endpoint must be an https URL")
	}
	return endpoint, nil
}

// validPushEndpoint is validEndpoint plus the allowlist, and it is what registration
// uses. Storing an endpoint is what makes this server post to it later, so the check
// belongs here rather than only at delivery: a refusal at subscribe time is a 400 the
// browser can report, where the same refusal at delivery time is a device that
// registered cleanly and then never receives anything.
//
// The rejection is logged at warn with the host, because the cost of an allowlist is
// paid on the day a browser starts minting endpoints somewhere new. That day, this line
// is the difference between a one-word fix in ALMANACK_PUSH_HOSTS and an afternoon
// wondering why one person's phone went quiet.
func (s *Server) validPushEndpoint(raw string) (string, error) {
	endpoint, err := validEndpoint(raw)
	if err != nil {
		return "", err
	}
	if !domain.PushEndpointAllowed(endpoint, s.cfg.PushHosts) {
		host := endpointHost(endpoint)
		slog.Warn("push endpoint refused: its host is not an allowed push service",
			"service", host, "setting", "ALMANACK_PUSH_HOSTS")
		return "", invalidf("%s is not a push service this server sends to; ask whoever runs it to allow that host", host)
	}
	return endpoint, nil
}

func endpointHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		return u.Host
	}
	return "?"
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 200 {
			fail(w, r, invalidf("limit must be between 1 and 200"))
			return
		}
		limit = n
	}
	var beforeID int64
	if raw := strings.TrimSpace(q.Get("before_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			fail(w, r, invalidf("before_id must be an activity id"))
			return
		}
		beforeID = id
	}
	// before= is the instant cursor this endpoint shipped with. It cannot page
	// through a shared second — that is the bug before_id exists to fix — but it is
	// still answered, exactly as it always was, so that a client written against the
	// older documentation is not broken by the fix.
	var before time.Time
	if raw := strings.TrimSpace(q.Get("before")); raw != "" && beforeID == 0 {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			fail(w, r, invalidf("before must be an RFC 3339 instant"))
			return
		}
		before = t
	}

	ctx := r.Context()
	user := userOf(ctx)
	cals, err := s.store.ListCalendarsForUser(ctx, user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	var entries []domain.Activity
	if before.IsZero() {
		entries, err = s.store.ListActivity(ctx, calendarIDs(cals), limit, beforeID)
	} else {
		entries, err = s.store.ListActivityBetween(ctx, calendarIDs(cals), time.Time{}, before, limit)
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	rows, err := s.activityViews(ctx, entries)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"activity": rows})
}

// activityView is one feed entry plus the date the row links to.
//
// The date is here for the same reason a search result carries one. A feed row about a
// recurring event has only the instant the change was made, and the client built its
// link from that — but GET /events/{id} answers 404 for a date the series does not land
// on, and the day somebody edited a swimming lesson is almost never a Tuesday. Tapping
// the row reported the event as missing. Deriving it on the server is what keeps the
// answer honest: a series' anchor need not be an occurrence of its own rule, so there is
// nothing the browser could correctly guess from what it holds.
//
// It is null when there is nothing to link to — a deleted event, or a rule with no
// occurrence anywhere — and the client renders those rows as plain text rather than as a
// link to an error.
type activityView struct {
	domain.Activity
	OccurrenceDate *domain.Date `json:"occurrence_date"`
}

// activityViews resolves the link date for a page of entries, reading each event once
// however many of its changes the page holds.
//
// In two passes rather than one, so that the exceptions of every series on the page are
// read together. Resolved row by row it would be a query per row for the events and
// another per row for their overrides; the events were already folded by id, and the
// overrides now go the same way.
func (s *Server) activityViews(ctx context.Context, entries []domain.Activity) ([]activityView, error) {
	// A nil value means "looked up, and it is gone" — distinct from "not looked up yet",
	// so an event deleted since is read once rather than once per change it left behind.
	loaded := map[int64]*domain.Event{}
	for _, a := range entries {
		if a.EventID == nil || a.Action == domain.ActionEventDeleted {
			continue // nothing to open: a deleted event is kept in the feed by its title
		}
		if _, done := loaded[*a.EventID]; done {
			continue
		}
		ev, err := s.store.EventByID(ctx, *a.EventID)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			loaded[*a.EventID] = nil
		case err != nil:
			return nil, err
		default:
			loaded[*a.EventID] = &ev
		}
	}

	present := make([]domain.Event, 0, len(loaded))
	for _, ev := range loaded {
		if ev != nil {
			present = append(present, *ev)
		}
	}
	overrides, err := s.store.OverridesOf(ctx, recurrenceIDsOf(present))
	if err != nil {
		return nil, err
	}
	cancelled := cancelledDates(overrides)

	today := domain.DateIn(s.clock.Now(), s.cfg.FamilyTZ)
	dates := map[int64]*domain.Date{}
	for id, ev := range loaded {
		if ev == nil {
			continue
		}
		var struck map[domain.Date]bool
		if ev.RecurrenceID != nil {
			struck = cancelled[*ev.RecurrenceID]
		}
		_, occurrence, err := s.resultDates(ctx, *ev, today, struck)
		if err != nil {
			return nil, err
		}
		dates[id] = occurrence
	}

	out := make([]activityView, 0, len(entries))
	for _, a := range entries {
		view := activityView{Activity: a}
		if a.EventID != nil && a.Action != domain.ActionEventDeleted {
			view.OccurrenceDate = dates[*a.EventID]
		}
		out = append(out, view)
	}
	return out, nil
}
