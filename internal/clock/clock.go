// Package clock provides the only sanctioned source of the current time.
//
// Nothing outside this package may call time.Now(). Everything that cares about
// "now" — the scheduler, the reminder planner, session expiry, token TTLs — takes a
// Clock. That makes the whole notification pipeline testable without sleeping, and
// lets dev mode travel forward a day to watch tomorrow's digest fire.
package clock

import (
	"sync"
	"time"
)

// Clock reports the current instant, always in UTC.
type Clock interface {
	Now() time.Time
}

// Real is the production clock.
type Real struct{}

func (Real) Now() time.Time { return time.Now().UTC() }

// Fake is a manually controlled clock for tests and dev mode. It is safe for
// concurrent use, because the scheduler goroutine reads it while a test advances it.
type Fake struct {
	mu  sync.RWMutex
	now time.Time
}

// NewFake returns a Fake set to t.
func NewFake(t time.Time) *Fake { return &Fake{now: t.UTC()} }

func (f *Fake) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.now
}

// Set moves the clock to t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC()
}

// Advance moves the clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d).UTC()
}
