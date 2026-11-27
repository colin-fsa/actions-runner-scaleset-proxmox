package pool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// WindowsBootBudget is shared by every profile and scale set. The historical
// name is retained for compatibility; the budget applies to all guests.
type WindowsBootBudget struct {
	limit               int64
	interval            time.Duration
	readyAt             time.Time
	verificationPending bool
	mu                  sync.Mutex
	nodes               map[string]*bootNode
}

type coldWaiter struct {
	clone bool
	ready bool
}

type bootNode struct {
	slots                *semaphore.Weighted
	coldWaiting          map[*coldWaiter]struct{}
	pace                 sync.Mutex
	probeMu              sync.Mutex
	coldProbe, warmProbe startProbe
	starts               submissionWindow
	clones               submissionWindow
	warmWaiting          atomic.Int64
	warmBurst            int64
}

func NewWindowsBootBudget(limit int) *WindowsBootBudget {
	return NewHostBootBudget(limit, 0, 0)
}

func NewHostBootBudget(limit int, interval, recovery time.Duration) *WindowsBootBudget {
	return &WindowsBootBudget{limit: int64(limit), interval: interval, readyAt: time.Now().Add(recovery), nodes: make(map[string]*bootNode)}
}
func (b *WindowsBootBudget) node(node string) *bootNode {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.nodes[node] == nil {
		b.nodes[node] = &bootNode{slots: semaphore.NewWeighted(b.limit), coldWaiting: make(map[*coldWaiter]struct{})}
	}
	return b.nodes[node]
}

// acquireClone is retained for caller compatibility. In paced mode the actual
// clone POST consumes a rolling-window entry, not a semaphore held through
// configuration/readiness. Cancellation before submission consumes nothing.
func (b *WindowsBootBudget) acquireClone(ctx context.Context, node string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return func() {}, nil
}

// submissionWindow counts submissions, even ambiguous HTTP outcomes, rather
// than their task completion. Caller holds bootNode.pace.
type submissionWindow struct{ times []time.Time }

func (w *submissionWindow) delay(now time.Time, limit int64, interval time.Duration) time.Duration {
	cut := now.Add(-interval)
	i := 0
	for i < len(w.times) && !w.times[i].After(cut) {
		i++
	}
	w.times = w.times[i:]
	if limit <= 0 || int64(len(w.times)) < limit {
		return 0
	}
	return max(w.times[0].Add(interval).Sub(now), 0)
}

func (w *submissionWindow) submitted(now time.Time) { w.times = append(w.times, now) }

// Caller holds pace. Cold timers do not earn a turn until their POST is due.
func (n *bootNode) coldDue(limit int64, interval time.Duration) bool {
	now := time.Now()
	for waiter := range n.coldWaiting {
		if !waiter.ready {
			continue
		}
		window := &n.starts
		if waiter.clone {
			window = &n.clones
		}
		if window.delay(now, limit, interval) == 0 {
			return true
		}
	}
	return false
}

func (b *WindowsBootBudget) acquire(ctx context.Context, node string) (func(), error) {
	if b == nil || b.limit <= 0 || b.UsesPacedAdmission() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return func() {}, nil
	}
	s := b.node(node).slots
	if err := s.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	return sync.OnceFunc(func() { s.Release(1) }), nil
}
func (b *WindowsBootBudget) tryAcquire(node string) (func(), bool) {
	if b == nil || (b.limit <= 0 && b.interval <= 0 && !b.RecoveryPending()) {
		return func() {}, true
	}
	b.mu.Lock()
	blocked := b.verificationPending || time.Now().Before(b.readyAt)
	b.mu.Unlock()
	if blocked {
		return nil, false
	}
	if b.limit <= 0 || b.UsesPacedAdmission() {
		return func() {}, true
	}
	s := b.node(node).slots
	if !s.TryAcquire(1) {
		return nil, false
	}
	return sync.OnceFunc(func() { s.Release(1) }), true
}

// RequireRecovery holds the boot gate until the leader proves its own
// outstanding Proxmox work has been reconciled.
func (b *WindowsBootBudget) RequireRecovery(pending bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.verificationPending = pending
}

func (b *WindowsBootBudget) RecoveryPending() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.verificationPending
}

// BeginRecovery suppresses new starts following each leader transition.
func (b *WindowsBootBudget) BeginRecovery(delay time.Duration) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.readyAt = time.Now().Add(delay)
	b.mu.Unlock()
}

// AcquireStart is used by paths outside pool promotion, including LXC JIT
// restarts. Waiting is cancellable and shares the same host slots.
func (b *WindowsBootBudget) AcquireStart(ctx context.Context, node string) (func(), error) {
	return b.acquireStart(ctx, node, nil)
}

func (b *WindowsBootBudget) acquireStart(ctx context.Context, node string, action func(markSubmitted func()) error) (func(), error) {
	for {
		release, ok := b.tryAcquire(node)
		if ok {
			if err := b.paceAction(ctx, node, action); err != nil {
				release()
				return nil, err
			}
			return release, nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// pace serializes the actual start calls, including delayed clones. Timing
// admission instead would allow slow clones to finish together and burst.
func (b *WindowsBootBudget) pace(ctx context.Context, node string) error {
	return b.paceAdmission(ctx, node, nil)
}

// UsesPacedAdmission means start work is bounded and interleaved.
func (b *WindowsBootBudget) UsesPacedAdmission() bool {
	return b != nil && b.limit > 0 && b.interval > 0
}

// AcquireStartWithSubmission holds the common pacer through the HTTP POST,
// but not the task wait. A call that may have reached PVE marks submission
// even when its HTTP response is an error.
func (b *WindowsBootBudget) AcquireStartWithSubmission(ctx context.Context, node string, action func(markSubmitted func()) error) (func(), error) {
	return b.acquireStart(ctx, node, action)
}

func (b *WindowsBootBudget) paceAdmission(ctx context.Context, node string, admit func() error) error {
	return b.paceAction(ctx, node, func(markSubmitted func()) error {
		if admit != nil {
			if err := admit(); err != nil {
				return err
			}
		}
		markSubmitted()
		return nil
	})
}

// paceAction retains spacing for cold power requests and JIT restarts.
func (b *WindowsBootBudget) paceAction(ctx context.Context, node string, action func(markSubmitted func()) error) error {
	return b.dispatch(ctx, node, false, false, action)
}

// warmAction bypasses cold pacing, but shares the POST boundary and the
// accountant's fresh actual-host pressure check. No readiness slot is held.
func (b *WindowsBootBudget) warmAction(ctx context.Context, node string, action func(func()) error) error {
	return b.dispatch(ctx, node, true, false, action)
}

func (b *WindowsBootBudget) dispatch(ctx context.Context, node string, warm, clone bool, action func(func()) error) error {
	return b.dispatchPrepared(ctx, node, warm, clone, nil, action)
}

func (b *WindowsBootBudget) dispatchPrepared(ctx context.Context, node string, warm, clone bool, prepare func(context.Context) error, action func(func()) error) error {
	if b == nil || b.interval <= 0 {
		if err := ctx.Err(); err != nil || action == nil {
			return err
		}
		return action(func() {})
	}
	cancelled := func() error {
		if prepare != nil {
			return errors.Join(errStartCapacityDeferred, ctx.Err())
		}
		return ctx.Err()
	}
	n := b.node(node)
	countedWarm := false
	var waiter *coldWaiter
	if !warm {
		waiter = &coldWaiter{clone: clone, ready: prepare == nil}
		n.pace.Lock()
		n.coldWaiting[waiter] = struct{}{}
		n.pace.Unlock()
		defer func() { n.pace.Lock(); delete(n.coldWaiting, waiter); n.pace.Unlock() }()
	}
	defer func() {
		if countedWarm {
			n.warmWaiting.Add(-1)
		}
	}()
	prepared := prepare == nil
	for {
		if !prepared {
			// Cold clocks still pace actual submissions. Do not fetch a sample
			// fifteen seconds before it could be used.
			n.pace.Lock()
			window := &n.starts
			if clone {
				window = &n.clones
			}
			delay := window.delay(time.Now(), b.limit, b.interval)
			if warm {
				delay = 0
			}
			n.pace.Unlock()
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return cancelled()
				case <-timer.C:
				}
			}
			err := prepare(ctx)
			if err != nil {
				var retry *startRetry
				if !warm && errors.As(err, &retry) {
					timer := time.NewTimer(max(time.Until(retry.at), time.Millisecond))
					select {
					case <-ctx.Done():
						timer.Stop()
						return errors.Join(errStartCapacityDeferred, ctx.Err())
					case <-timer.C:
					}
					continue
				}
				// Preparation never issued a POST or bought a charge.
				return errors.Join(errStartCapacityDeferred, err)
			}
			prepared = true
		}
		if warm && !countedWarm {
			n.warmWaiting.Add(1)
			countedWarm = true
		}
		n.pace.Lock()
		if waiter != nil {
			waiter.ready = true
		}

		window := &n.starts
		if clone {
			window = &n.clones
		}
		delay := window.delay(time.Now(), b.limit, b.interval)
		// Sleep outside the POST lock: a cold timer must not block a warm
		// promotion. Bound priority to one host-sized burst so cold work wins
		// a turn even with contemporaneous warm requests.
		coldTurn := warm && n.warmBurst >= b.limit && n.coldDue(b.limit, b.interval)
		if coldTurn || (!warm && (delay > 0 || (n.warmWaiting.Load() > 0 && n.warmBurst < b.limit))) {
			n.pace.Unlock()
			if warm || delay <= 0 {
				delay = time.Millisecond
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return cancelled()
			case <-timer.C:
			}
			continue
		}
		err := func() error {
			defer n.pace.Unlock()
			if ctx.Err() != nil {
				return cancelled()
			}
			// Recheck the leader fence after waiting, including for clone POSTs.
			b.mu.Lock()
			blocked := b.verificationPending || time.Now().Before(b.readyAt)
			b.mu.Unlock()
			if blocked {
				return errStartCapacityDeferred
			}
			// A refused warm admission also used its priority turn; a busy
			// ledger must not let a stream of refusals starve cold requests.
			if warm {
				n.warmBurst = min(b.limit, n.warmBurst+1)
			} else {
				n.warmBurst = 0
			}
			submitted := false
			var err error
			if action == nil {
				submitted = true
			} else {
				err = action(func() { submitted = true })
			}
			if submitted && !warm {
				window.submitted(time.Now())
			}
			return err
		}()
		var retry *startRetry
		if errors.As(err, &retry) && prepare != nil {
			if warm {
				return errors.Join(errStartCapacityDeferred, err)
			}
			n.pace.Lock()
			waiter.ready = false
			n.pace.Unlock()
			prepared = false
			timer := time.NewTimer(max(time.Until(retry.at), time.Millisecond))
			select {
			case <-ctx.Done():
				timer.Stop()
				return errors.Join(errStartCapacityDeferred, ctx.Err())
			case <-timer.C:
			}
			continue
		}
		return err
	}
}
