package pool

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// WindowsBootBudget is shared by every profile and scale set. The historical
// name is retained for compatibility; the budget applies to all guests.
type WindowsBootBudget struct {
	limit    int64
	interval time.Duration
	readyAt  time.Time
	mu       sync.Mutex
	nodes    map[string]*bootNode
}

type bootNode struct {
	slots *semaphore.Weighted
	pace  sync.Mutex
	last  time.Time
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
	if b == nil || (b.limit <= 0 && b.interval <= 0) {
		return func() {}, true
	}
	b.mu.Lock()
	blocked := time.Now().Before(b.readyAt)
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
	for {
		release, ok := b.tryAcquire(node)
		if ok {
			if err := b.pace(ctx, node); err != nil {
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
	if b == nil || b.interval <= 0 {
		return ctx.Err()
	}
	n := b.node(node)
	n.pace.Lock()
	defer n.pace.Unlock()
	if delay := time.Until(n.last.Add(b.interval)); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	n.last = time.Now()
	return nil
}
