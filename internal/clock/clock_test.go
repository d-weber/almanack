package clock

import (
	"sync"
	"testing"
	"time"
)

// Both implementations must satisfy the interface with the receivers their call sites
// use: serve.go hands over a clock.Real value, and NewFake returns a *Fake.
var (
	_ Clock = Real{}
	_ Clock = (*Fake)(nil)
)

// Paris is the family timezone in the default configuration, and the one whose summer
// offset is +02:00 — enough to move an instant into the previous or next day, which is
// the bug a non-UTC clock would cause.
func paris(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skipf("Europe/Paris unavailable: %v", err)
	}
	return loc
}

// TestRealNowIsUTC pins the half of the contract that cannot be tested by inspection.
// Everything downstream — ISO-8601 storage with a Z, date bucketing in the family
// timezone, digest membership — starts from this instant and assumes it carries no
// local offset. A Real that returned time.Now() unconverted would keep every test
// green on a machine set to UTC and put the evening events on the wrong day everywhere
// else.
func TestRealNowIsUTC(t *testing.T) {
	var c Clock = Real{}

	before := time.Now().UTC()
	got := c.Now()
	after := time.Now().UTC()

	if got.Location() != time.UTC {
		t.Errorf("Real.Now().Location() = %v, want UTC", got.Location())
	}
	if _, offset := got.Zone(); offset != 0 {
		t.Errorf("Real.Now() carries a %d-second offset, want 0", offset)
	}
	if got.Before(before) || got.After(after) {
		t.Errorf("Real.Now() = %v, which is outside [%v, %v]", got, before, after)
	}
	// Not a clock at all if it stands still: two reads a few instructions apart
	// must not go backwards.
	if second := c.Now(); second.Before(got) {
		t.Errorf("Real.Now() went backwards: %v then %v", got, second)
	}
}

// TestFakeNormalisesToUTC covers the same contract for the test and dev-mode clock.
// A fake constructed from a wall-clock time in the family timezone must behave exactly
// like the real one, or a test passes against an instant the production clock could
// never produce — and `POST /dev/clock` sets this thing from whatever the developer
// typed.
func TestFakeNormalisesToUTC(t *testing.T) {
	loc := paris(t)

	// 00:30 Paris in summer is 22:30 UTC the previous day: the case where a clock
	// that kept its offset would land on a different date.
	cases := []struct {
		name  string
		given time.Time
		want  string
	}{
		{"already utc", time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC), "2026-07-26T09:00:00Z"},
		{"paris in summer", time.Date(2026, 7, 26, 0, 30, 0, 0, loc), "2026-07-25T22:30:00Z"},
		{"paris in winter", time.Date(2026, 1, 15, 0, 30, 0, 0, loc), "2026-01-14T23:30:00Z"},
		{"a fixed zone west of utc", time.Date(2026, 3, 1, 6, 0, 0, 0, time.FixedZone("EST", -5*3600)), "2026-03-01T11:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for label, got := range map[string]time.Time{
				"NewFake": NewFake(tc.given).Now(),
				"Set":     setTo(tc.given).Now(),
			} {
				if got.Location() != time.UTC {
					t.Errorf("%s: location = %v, want UTC", label, got.Location())
				}
				if formatted := got.Format(time.RFC3339); formatted != tc.want {
					t.Errorf("%s: Now() = %s, want %s", label, formatted, tc.want)
				}
				// Normalising must move the label, not the instant.
				if !got.Equal(tc.given) {
					t.Errorf("%s: Now() = %v, which is a different instant from %v", label, got, tc.given)
				}
			}
		})
	}
}

func setTo(when time.Time) *Fake {
	f := NewFake(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	f.Set(when)
	return f
}

// TestFakeAdvance covers time travel, which is how the notification pipeline is tested
// without sleeping: the scheduler is told it is tomorrow and asked what it would send.
func TestFakeAdvance(t *testing.T) {
	start := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		by   []time.Duration
		want time.Time
	}{
		{"zero", []time.Duration{0}, start},
		{"a minute", []time.Duration{time.Minute}, time.Date(2026, 7, 26, 9, 1, 0, 0, time.UTC)},
		{"a day", []time.Duration{24 * time.Hour}, time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)},
		{"successive advances accumulate", []time.Duration{time.Hour, 30 * time.Minute, 30 * time.Minute}, time.Date(2026, 7, 26, 11, 0, 0, 0, time.UTC)},
		// Backwards is allowed: a test that wants to replay a window rewinds
		// rather than rebuilding its fixtures.
		{"backwards", []time.Duration{-2 * time.Hour}, time.Date(2026, 7, 26, 7, 0, 0, 0, time.UTC)},
		{"across a month boundary", []time.Duration{6 * 24 * time.Hour}, time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)},
		// UTC has no summer time, so crossing the last Sunday in October must not
		// shift the wall clock by an hour. This is the assertion that would fail if
		// Fake ever stored its instant in a real zone.
		{"across the european dst change", []time.Duration{96 * 24 * time.Hour}, time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFake(start)
			for _, d := range tc.by {
				f.Advance(d)
			}
			if got := f.Now(); !got.Equal(tc.want) || got.Location() != time.UTC {
				t.Errorf("Now() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFakeNowIsStable guards against a Now() that reads the wall clock behind the
// caller's back. Nothing may move except Set and Advance, or a test that asserts on
// two timestamps a millisecond apart becomes a coin toss on a slow machine.
func TestFakeNowIsStable(t *testing.T) {
	f := NewFake(time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC))
	first := f.Now()
	for i := 0; i < 1000; i++ {
		if got := f.Now(); !got.Equal(first) {
			t.Fatalf("Now() moved from %v to %v without being told to", first, got)
		}
	}
	// A monotonic reading picked up from time.Now() would survive into comparisons
	// and make two equal instants unequal; UTC() strips it, and this is what says so.
	if f.Now().Round(0) != f.Now() {
		t.Error("Now() carries a monotonic clock reading")
	}
}

// TestFakeIsSafeForConcurrentUse is the reason Fake holds a mutex at all: in the
// notification tests the scheduler goroutine reads the clock while the test advances
// it. Run under `make race`, this is what catches a Fake that dropped its lock.
func TestFakeIsSafeForConcurrentUse(t *testing.T) {
	const writers, readsPerWriter = 8, 200

	start := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	f := NewFake(start)

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < readsPerWriter; j++ {
				f.Advance(time.Second)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < readsPerWriter; j++ {
				// Every read must land on a whole second: a torn read would
				// mean the instant was assembled from two different writes.
				if now := f.Now(); now.Nanosecond() != 0 || now.Before(start) {
					t.Errorf("Now() = %v, which is not a whole second at or after %v", now, start)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Every increment has to have landed: a read-modify-write outside the lock
	// loses some of them.
	if want := start.Add(writers * readsPerWriter * time.Second); !f.Now().Equal(want) {
		t.Errorf("after %d concurrent advances Now() = %v, want %v", writers*readsPerWriter, f.Now(), want)
	}
}
