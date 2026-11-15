package contracts

import "time"

// Clock is the time storage stamps and the lifecycle reads and waits on, so
// a test can move it.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the time once d has passed.
	After(d time.Duration) <-chan time.Time
}

// SystemClock is the system's clock.
var SystemClock Clock = systemClock{}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
