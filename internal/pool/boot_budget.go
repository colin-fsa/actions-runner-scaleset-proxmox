package pool

import (
	"context"
	"sync"

	"golang.org/x/sync/semaphore"
)

// WindowsBootBudget limits simultaneous Windows starts on each node, shared
// across scale sets. Zero disables pacing. It does not cap steady-state VMs.
type WindowsBootBudget struct {
	limit int64
	mu    sync.Mutex
	nodes map[string]*semaphore.Weighted
}

func NewWindowsBootBudget(limit int) *WindowsBootBudget {
	return &WindowsBootBudget{limit: int64(limit), nodes: make(map[string]*semaphore.Weighted)}
}
func (b *WindowsBootBudget) node(node string) *semaphore.Weighted {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.nodes[node] == nil {
		b.nodes[node] = semaphore.NewWeighted(b.limit)
	}
	return b.nodes[node]
}
func (b *WindowsBootBudget) acquire(ctx context.Context, node string) (func(), error) {
	if b == nil || b.limit <= 0 {
		return func() {}, nil
	}
	s := b.node(node)
	if err := s.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	return sync.OnceFunc(func() { s.Release(1) }), nil
}
func (b *WindowsBootBudget) tryAcquire(node string) (func(), bool) {
	if b == nil || b.limit <= 0 {
		return func() {}, true
	}
	s := b.node(node)
	if !s.TryAcquire(1) {
		return nil, false
	}
	return sync.OnceFunc(func() { s.Release(1) }), true
}
