//go:build e2e

package e2e

import (
	"sync"
	"testing"
	"time"
)

// lifecycleEpoch is when the lifecycle tests' clock starts: the creation
// time of the S3 User Guide's example ("Lifecycle configuration
// elements"), whose object, given 3 days, expires on 1/19/2014 at 00:00
// UTC.
var lifecycleEpoch = time.Date(2014, time.January, 15, 10, 30, 0, 0, time.UTC)

// fakeClock is the clock object-store stamps what it stores with and runs
// the lifecycle on in these tests: it starts at a fixed time and moves
// only when the test advances it. It is secret-store's rotation test clock
// with its start time given.
type fakeClock struct {
	now     time.Time
	waiters []clockWaiter
	mu      sync.Mutex
}

type clockWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock(at time.Time) *fakeClock {
	return &fakeClock{now: at}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, clockWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock on by d and ends every wait that is over.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = kept
}

// waiting is how many waits are pending: the lifecycle scheduler's, the
// only one, while it is parked between two ticks.
func (c *fakeClock) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// advance moves clk to to once the lifecycle scheduler is parked, and
// returns once it is parked again: after the tick the move started, if it
// ended the scheduler's wait, so the state a test then reads is the one
// that tick left. One server at a time waits on clk; a server stopped
// leaves its wait behind, so a server started again gets a clock of its
// own.
func advance(t *testing.T, clk *fakeClock, to time.Time) {
	t.Helper()
	waitParked(t, clk)
	d := to.Sub(clk.Now())
	if d < 0 {
		t.Fatalf("advance to %s: the clock is already at %s", to, clk.Now())
	}
	clk.Advance(d)
	waitParked(t, clk)
}

// waitParked waits until the lifecycle scheduler waits on clk.
func waitParked(t *testing.T, clk *fakeClock) {
	t.Helper()
	eventually(t, "the lifecycle scheduler to wait on the clock",
		func() bool { return clk.waiting() > 0 })
}

// eventuallyTimeout is how long eventually polls for.
const eventuallyTimeout = 10 * time.Second

// eventually polls cond until it holds, failing the test after
// eventuallyTimeout.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(eventuallyTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", eventuallyTimeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
