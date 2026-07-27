package domain

import "testing"

func intp(v int) *int { return &v }

// A reminder's shape is its identity, and two things follow that the rest of the app
// leans on: two reminders that say the same thing share it whatever else is on the
// struct, and two that say different things never do. internal/store reconciles a saved
// list against the stored rows by it, and internal/httpapi folds a list down to a set by
// it, so a collision would silently drop a warning somebody asked for.
func TestReminderShape(t *testing.T) {
	tests := []struct {
		name string
		r    Reminder
		want string
	}{
		{"at the start", Reminder{OffsetMinutes: intp(0)}, "m0"},
		{"half an hour before", Reminder{OffsetMinutes: intp(30)}, "m30"},
		{"the longest offset the API accepts", Reminder{OffsetMinutes: intp(60 * 24 * 30)}, "m43200"},
		{"on the day", Reminder{DaysBefore: intp(0), AtTimeLocal: "09:00"}, "d0@09:00"},
		{"the day before", Reminder{DaysBefore: intp(1), AtTimeLocal: "09:00"}, "d1@09:00"},
		{"the evening before", Reminder{DaysBefore: intp(1), AtTimeLocal: "20:00"}, "d1@20:00"},
		// Nothing may store a row with neither shape — the table's CHECK refuses it —
		// so this is the answer on a database somebody has taken the constraint off.
		{"neither shape", Reminder{}, ""},
		// Nor with both. The offset wins, so that such a row still has one answer
		// rather than an answer that depends on who asked.
		{"both shapes", Reminder{OffsetMinutes: intp(45), DaysBefore: intp(1), AtTimeLocal: "09:00"}, "m45"},
	}
	seen := map[string]string{}
	for _, tt := range tests {
		if got := tt.r.Shape(); got != tt.want {
			t.Errorf("%s: Shape() = %q, want %q", tt.name, got, tt.want)
		}
		if prev, dup := seen[tt.want]; dup {
			t.Errorf("%s and %s share the shape %q, so nothing can tell them apart", prev, tt.name, tt.want)
		}
		seen[tt.want] = tt.name
	}

	// The shape is what a reminder says, not which row it is or whose it is. That is
	// the whole of why a list can be folded to a set and matched against stored rows:
	// the copy that arrives in a request carries none of these.
	stored := Reminder{ID: 7, EventID: intp64(3), UserID: 2, OffsetMinutes: intp(30)}
	fromRequest := Reminder{OffsetMinutes: intp(30)}
	if stored.Shape() != fromRequest.Shape() {
		t.Errorf("a stored reminder and the request asking for it differ: %q vs %q",
			stored.Shape(), fromRequest.Shape())
	}
}

func intp64(v int64) *int64 { return &v }
