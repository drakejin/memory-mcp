package hotstore

import "time"

// Clock abstracts wall-clock time so aging, TTL and freshness logic stays
// testable (code-standards §1.1). It is the single shared clock interface of
// the project: consumer packages depend on this narrow shape, not on a store.
type Clock interface {
	Now() time.Time
}

// systemClock is the production Clock — the only place time.Now() is called.
type systemClock struct{}

// Now returns the current wall-clock time.
func (systemClock) Now() time.Time { return time.Now() }

// NewSystemClock returns the production Clock.
func NewSystemClock() Clock { return systemClock{} }
