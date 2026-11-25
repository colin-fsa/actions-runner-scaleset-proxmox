package pool

import (
	"context"
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

type bootNode struct {
	slots       *semaphore.Weighted
	pace        sync.Mutex
	last        time.Time
	lastClone   time.Time
	warmWaiting atomic.Int64
	warmBurst   int64
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
		b.nodes[node] = &bootNode{slots: semaphore.NewWeighted(b.limit)}
	}
	return b.nodes[node]
}
func (b *WindowsBootBudget) acquire(ctx context.Context, node string) (func(), error) {
	if b == nil || b.limit <= 0 {
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
	if b.limit <= 0 {
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
// accountant's atomic outstanding-start limit. No readiness slot is held.
func (b *WindowsBootBudget) warmAction(ctx context.Context, node string, action func(func()) error) error {
	return b.dispatch(ctx, node, true, false, action)
}

func (b *WindowsBootBudget) dispatch(ctx context.Context, node string, warm, clone bool, action func(func()) error) error {
	if b == nil || b.interval <= 0 {
		if err := ctx.Err(); err != nil || action == nil {
			return err
		}
		return action(func() {})
	}
	n := b.node(node)
	if warm {
		n.warmWaiting.Add(1)
		defer n.warmWaiting.Add(-1)
	}
	for {
		n.pace.Lock()
		last := &n.last
		if clone {
			last = &n.lastClone
		}
		delay := time.Until(last.Add(b.interval))
		// Sleep outside the POST lock: a cold timer must not block a warm
		// promotion. Bound priority to one host-sized burst so cold work wins
		// a turn even with contemporaneous warm requests.
		if !warm && (delay > 0 || (n.warmWaiting.Load() > 0 && n.warmBurst < b.limit)) {
			n.pace.Unlock()
			if delay <= 0 {
				delay = time.Millisecond
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		err := func() error {
			defer n.pace.Unlock()
			if err := ctx.Err(); err != nil {
				return err
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
			}
			submitted := false
			var err error
			if action == nil {
				submitted = true
			} else {
				err = action(func() { submitted = true })
			}
			if submitted {
				if !warm {
					*last = time.Now()
					n.warmBurst = 0
				}
			}
			return err
		}()
		return err
	}
}
