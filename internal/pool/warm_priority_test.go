package pool

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/store"
	"github.com/stretchr/testify/require"
)

func TestWarmBypassesColdTimerAndColdStillProgresses(t *testing.T) {
	const interval = 80 * time.Millisecond
	b := NewHostBootBudget(2, interval, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	// Start a cold POST, then keep another cold request waiting on its clock.
	require.NoError(t, b.paceAction(ctx, "pve1", nil))
	firstReturned := time.Now()
	cold := make(chan time.Time, 1)
	go func() {
		_ = b.paceAction(ctx, "pve1", func(mark func()) error { mark(); cold <- time.Now(); return nil })
	}()
	// Warm requests ignore the 15s-equivalent timer, including while a cold
	// waiter is sleeping; cold requests keep their original minimum spacing.
	for range 2 {
		require.NoError(t, b.warmAction(ctx, "pve1", func(mark func()) error { mark(); return nil }))
	}
	warmReturned := time.Now()
	require.Less(t, warmReturned.Sub(firstReturned), interval)
	select {
	case at := <-cold:
		require.GreaterOrEqual(t, at.Sub(firstReturned), interval-time.Millisecond)
		t.Logf("local gate replay: 2 warm POSTs in %s; next cold POST at %s (configured interval %s)", warmReturned.Sub(firstReturned), at.Sub(firstReturned), interval)
	case <-ctx.Done():
		t.Fatal("cold work starved")
	}
}

func TestWarmPriorityForContemporaneousSubmission(t *testing.T) {
	b := NewHostBootBudget(2, time.Nanosecond, 0)
	n := b.node("pve1")
	n.pace.Lock()
	var mu sync.Mutex
	var order []string
	submit := func(kind string) func(func()) error {
		return func(mark func()) error { mark(); mu.Lock(); order = append(order, kind); mu.Unlock(); return nil }
	}
	done := make(chan error, 2)
	go func() { done <- b.paceAction(t.Context(), "pve1", submit("cold")) }()
	go func() { done <- b.warmAction(t.Context(), "pve1", submit("warm")) }()
	require.Eventually(t, func() bool { return n.warmWaiting.Load() == 1 }, time.Second, time.Millisecond)
	n.pace.Unlock()
	require.NoError(t, <-done)
	require.NoError(t, <-done)
	require.Equal(t, []string{"warm", "cold"}, order)
}

func TestWarmAndCloneSubmissionRespectRecoveryAndCancellation(t *testing.T) {
	for _, warm := range []bool{false, true} {
		b := NewHostBootBudget(2, time.Nanosecond, 0)
		b.RequireRecovery(true)
		called := false
		action := func(func()) error { called = true; return nil }
		require.ErrorIs(t, b.dispatch(t.Context(), "pve1", warm, !warm, action), errStartCapacityDeferred)
		require.False(t, called)
		b.RequireRecovery(false)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t, b.dispatch(ctx, "pve1", warm, !warm, action), context.Canceled)
		require.False(t, called)
		require.NoError(t, b.dispatch(t.Context(), "pve1", warm, !warm, action))
		require.True(t, called)
	}
}

func TestClonePostClockIndependentOfWarmAndColdPower(t *testing.T) {
	const interval = 30 * time.Millisecond
	b := NewHostBootBudget(2, interval, 0)
	require.NoError(t, b.dispatch(t.Context(), "pve1", false, true, nil))
	first := time.Now()
	require.NoError(t, b.warmAction(t.Context(), "pve1", func(mark func()) error { mark(); return nil }))
	require.NoError(t, b.paceAction(t.Context(), "pve1", nil))
	require.NoError(t, b.dispatch(t.Context(), "pve1", false, true, nil))
	require.GreaterOrEqual(t, time.Since(first), interval-time.Millisecond)
}

type stoppedCloneBarrier struct {
	*heldBootProv
	cloneEntered chan struct{}
	cloneRelease chan struct{}
}

func (p *stoppedCloneBarrier) Clone(ctx context.Context, opts provisioner.CloneOptions) (*provisioner.VM, error) {
	close(p.cloneEntered)
	select {
	case <-p.cloneRelease:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return p.fakeProv.Clone(ctx, opts)
}

func TestPacedCloneDoesNotOccupyWarmReadinessSlots(t *testing.T) {
	st := newTestStore(t)
	adm := &pacedAdmitterStub{fakeAdmitter: newFakeAdmitter(map[string]uint64{"pve1": 32 * gib}), allow: true}
	p := &stoppedCloneBarrier{heldBootProv: &heldBootProv{fakeProv: &fakeProv{}, entered: make(chan int, 4), release: make(chan struct{})}, cloneEntered: make(chan struct{}), cloneRelease: make(chan struct{})}
	b := NewHostBootBudget(1, time.Hour, 0)
	m := newTestManager(t, st, p, Config{Capacity: adm, WindowsBootBudget: b, Profiles: []ProfileSettings{{Name: "p", MemoryMB: 4096, MaxConcurrentRunners: 10}}})
	t.Cleanup(m.Close)
	seedWarmRow(t, st, 60001, "p")
	done := make(chan struct{})
	go func() { defer close(done); m.runClone("p", store.PoolKindHot, true, nil, func() {}) }()
	<-p.cloneEntered
	// Saturate the legacy host slot too: the accountant, rather than a slot
	// held by a readiness worker, now bounds starts in the paced path.
	release, ok := b.tryAcquire("pve1")
	require.True(t, ok)
	defer release()
	m.promoteN(t.Context(), "p", 1)
	select {
	case id := <-p.entered:
		require.Equal(t, 60001, id)
	case <-time.After(time.Second):
		t.Fatal("warm blocked behind cloning or a readiness slot")
	}
	close(p.release)
	m.wg.Wait()
	close(p.cloneRelease)
	<-done
}

func TestColdWaiterRechecksLeaderFenceBeforeSubmission(t *testing.T) {
	b := NewHostBootBudget(2, 30*time.Millisecond, 0)
	require.NoError(t, b.paceAction(t.Context(), "pve1", nil))
	// Recovery begins after an earlier POST, while the next POST must wait.
	b.RequireRecovery(true)
	submitted := false
	require.ErrorIs(t, b.paceAction(t.Context(), "pve1", func(mark func()) error { submitted = true; mark(); return nil }), errStartCapacityDeferred)
	require.False(t, submitted)
	b.RequireRecovery(false)
	require.NoError(t, b.warmAction(t.Context(), "pve1", func(mark func()) error { mark(); return nil }))
}

type cloneGateProv struct {
	*fakeProv
	before    func(provisioner.CloneOptions)
	submitted bool
}

func (p *cloneGateProv) Clone(ctx context.Context, opts provisioner.CloneOptions) (*provisioner.VM, error) {
	p.before(opts)
	if err := opts.Submission(ctx, func(context.Context) error { p.submitted = true; return nil }); err != nil {
		return nil, err
	}
	return p.fakeProv.Clone(ctx, opts)
}

func TestDelayedCloneCannotSubmitForReplacedGeneration(t *testing.T) {
	st := newTestStore(t)
	adm := &pacedAdmitterStub{fakeAdmitter: newFakeAdmitter(map[string]uint64{"pve1": 32 * gib}), allow: true}
	p := &cloneGateProv{fakeProv: &fakeProv{}}
	var replacement *store.VM
	p.before = func(opts provisioner.CloneOptions) {
		old, err := st.Get(opts.NewVMID)
		require.NoError(t, err)
		require.NoError(t, st.Delete(opts.NewVMID))
		replacement = old
		replacement.CreatedAt = old.CreatedAt.Add(time.Second)
		replacement.Name = "replacement"
		require.NoError(t, st.Insert(replacement))
	}
	m := newTestManager(t, st, p, Config{Capacity: adm, WindowsBootBudget: NewHostBootBudget(2, time.Nanosecond, 0), Profiles: []ProfileSettings{{Name: "p", MemoryMB: 4096, MaxConcurrentRunners: 10}}})
	t.Cleanup(m.Close)
	m.runClone("p", store.PoolKindHot, true, nil, func() {})
	require.False(t, p.submitted)
	current, err := st.Get(replacement.VMID)
	require.NoError(t, err)
	require.Equal(t, replacement, current, "stale clone failure must not clean up the replacement")
}
