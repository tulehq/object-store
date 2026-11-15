package notify

import (
	"context"
	"sync"
)

// runner holds the deliverer's goroutines from begin to stop. gas's
// RestartService calls Init again on the same service after Close, and
// begin starts a new run.
type runner struct {
	ctx    context.Context //nolint:containedctx // the run's lifetime, ended by stop
	cancel context.CancelFunc
	// wg counts this run's goroutines: each run has its own, so a
	// goroutine of a run whose stop gave up waiting never shares a count
	// with the next run.
	wg *sync.WaitGroup
	mu sync.Mutex
}

// newRunner returns a runner that is stopped until begin.
func newRunner() *runner {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return &runner{ctx: ctx, cancel: cancel, wg: &sync.WaitGroup{}}
}

// begin starts a run.
func (r *runner) begin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancel()
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.wg = &sync.WaitGroup{}
}

// goBackground runs fn on a goroutine of this run, which stop ends and
// waits for, and reports whether it did: once stopped it starts nothing.
func (r *runner) goBackground(fn func(ctx context.Context)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return false
	}
	ctx, wg := r.ctx, r.wg
	wg.Add(1)
	go func() {
		defer wg.Done()
		fn(ctx)
	}()
	return true
}

// stop ends the run and waits for its goroutines, up to ctx's end.
func (r *runner) stop(ctx context.Context) error {
	r.mu.Lock()
	r.cancel()
	wg := r.wg
	r.mu.Unlock()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// Goroutines that stopped as ctx ended count as stopped.
		select {
		case <-done:
			return nil
		default:
			return ctx.Err() //nolint:wrapcheck // Close wraps it
		}
	}
}
