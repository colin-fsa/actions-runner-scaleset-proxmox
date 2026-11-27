package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/store"
	"github.com/stretchr/testify/require"
)

type heldBootProv struct {
	*fakeProv
	entered chan int
	release chan struct{}
}

func (p *heldBootProv) WaitReady(ctx context.Context, vm *provisioner.VM, _ time.Duration) error {
	p.entered <- vm.VMID
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestWindowsBudgetColdAndWarmShareNode(t *testing.T) {
	p := &heldBootProv{fakeProv: &fakeProv{}, entered: make(chan int, 8), release: make(chan struct{})}
	st := newTestStore(t)
	m := newTestManager(t, st, p, Config{WindowsBootBudget: NewWindowsBootBudget(1), Profiles: []ProfileSettings{
		{Name: "win", GuestOS: "windows", MaxConcurrentRunners: 10},
		{Name: "linux", GuestOS: "linux", MaxConcurrentRunners: 10},
	}})
	seedWarmRow(t, st, 60001, "win")
	seedWarmRow(t, st, 60002, "linux")
	// A hot clone holds the pve1 slot from before Clone through readiness.
	done := make(chan struct{})
	go func() { defer close(done); m.runClone("win", store.PoolKindHot, true, nil, func() {}) }()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("clone did not reach readiness")
	}
	m.promoteN(context.Background(), "win", 1)
	require.Equal(t, store.StateWarm, stateOf(t, st, 60001))
	// Linux shares the same host budget as the Windows cold clone.
	m.promoteN(context.Background(), "linux", 1)
	require.Equal(t, store.StateWarm, stateOf(t, st, 60002))
	// A Windows warm on a different node gets an independent slot.
	require.NoError(t, st.Insert(&store.VM{VMID: 60003, Node: "pve2", Name: "gh-runner-test-other", Profile: "win", State: store.StateWarm, PoolKind: store.PoolKindWarm}))
	m.promoteN(context.Background(), "win", 2)
	select {
	case id := <-p.entered:
		require.Equal(t, 60003, id)
	case <-time.After(5 * time.Second):
		t.Fatal("other node blocked")
	}
	close(p.release)
	<-done
	m.wg.Wait()
	m.promoteN(context.Background(), "win", 1)
	// slot returned: promotion reaches WaitReady; release it.
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("slot not returned")
	}
	m.wg.Wait()
}

func TestWindowsBudgetReleaseAndCancellation(t *testing.T) {
	b := NewWindowsBootBudget(1)
	release, ok := b.tryAcquire("a")
	require.True(t, ok)
	_, ok = b.tryAcquire("a")
	require.False(t, ok)
	other, ok := b.tryAcquire("b")
	require.True(t, ok)
	other()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.acquire(ctx, "a")
	require.ErrorIs(t, err, context.Canceled)
	release()
	release() // idempotent release must not panic or over-release
	again, ok := b.tryAcquire("a")
	require.True(t, ok)
	_, ok = b.tryAcquire("a")
	require.False(t, ok)
	again()
}

func TestWindowsBudgetReleasedOnPromotionStartFailure(t *testing.T) {
	b := NewWindowsBootBudget(1)
	st := newTestStore(t)
	p := &fakeProv{startErr: errors.New("start failed")}
	m := newTestManager(t, st, p, Config{WindowsBootBudget: b, Profiles: []ProfileSettings{{Name: "win", GuestOS: "windows", MaxConcurrentRunners: 10}}})
	seedWarmRow(t, st, 60010, "win")
	m.promoteN(context.Background(), "win", 1)
	m.wg.Wait()
	release, ok := b.tryAcquire("pve1")
	require.True(t, ok)
	release()
}

func TestWindowsBudgetReleasedOnCloneFailure(t *testing.T) {
	b := NewWindowsBootBudget(1)
	st := newTestStore(t)
	p := &fakeProv{cloneErr: errors.New("clone failed")}
	m := newTestManager(t, st, p, Config{WindowsBootBudget: b, Profiles: []ProfileSettings{{Name: "win", GuestOS: "windows", MaxConcurrentRunners: 10}}})
	m.runClone("win", store.PoolKindHot, true, nil, func() {})
	release, ok := b.tryAcquire("pve1")
	require.True(t, ok)
	release()
}

func TestHostBudgetSpacingAndRecovery(t *testing.T) {
	b := NewHostBootBudget(2, 30*time.Millisecond, 0)
	b.BeginRecovery(time.Hour)
	_, ok := b.tryAcquire("pve1")
	require.False(t, ok)
	b.BeginRecovery(0)
	release, ok := b.tryAcquire("pve1")
	require.True(t, ok)
	defer release()
	require.NoError(t, b.pace(context.Background(), "pve1"))
	start := time.Now()
	require.NoError(t, b.pace(context.Background(), "pve2"))
	require.NoError(t, b.pace(context.Background(), "pve1"))
	require.NoError(t, b.pace(context.Background(), "pve1"))
	require.GreaterOrEqual(t, time.Since(start), 25*time.Millisecond)
}

func TestBootBudgetRequiresEvidenceToEndRestartRecovery(t *testing.T) {
	b := NewHostBootBudget(2, time.Nanosecond, 0)
	b.RequireRecovery(true)
	b.BeginRecovery(0)
	_, ok := b.tryAcquire("pve1")
	require.False(t, ok, "expiring an old compatibility timer cannot clear proof gate")
	b.RequireRecovery(false)
	release, ok := b.tryAcquire("pve1")
	require.True(t, ok)
	release()
}
