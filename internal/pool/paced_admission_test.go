package pool

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/nodecap"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/store"
)

type pacedAdmitterStub struct {
	*fakeAdmitter
	allow   bool
	calls   int
	onCheck func()
}

func (*pacedAdmitterStub) SupportsPacedStart() bool { return true }
func (a *pacedAdmitterStub) ReserveExistingForPacedStart(_ context.Context, node string, shape nodecap.Shape, vmid int, _ time.Time, _ bool) (nodecap.Reservation, bool, error) {
	a.calls++
	if a.onCheck != nil {
		defer a.onCheck()
	}
	if !a.allow {
		return nil, false, nil
	}
	res, ok, err := a.reserve(node, shape, nil)
	res.Bind(vmid)
	return res, ok, err
}

func TestPacedColdStartCommitsOnlyAfterStoppedClone(t *testing.T) {
	for _, allow := range []bool{false, true} {
		st := newTestStore(t)
		adm := &pacedAdmitterStub{fakeAdmitter: newFakeAdmitter(map[string]uint64{"pve1": 32 * gib}), allow: allow}
		prov := &fakeProv{}
		prov.onClone = func(opts provisioner.CloneOptions) {
			require.False(t, opts.PoweredOn)
			require.Equal(t, uint64(32*gib), adm.snapshotFree("pve1"), "stopped clone holds no launch shape")
			require.Zero(t, adm.calls)
		}
		m := newTestManager(t, st, prov, Config{
			Profiles: []ProfileSettings{{Name: "p", MemoryMB: 10240, MaxConcurrentRunners: 10}},
			Capacity: adm, WindowsBootBudget: NewHostBootBudget(2, time.Millisecond, 0),
		})
		t.Cleanup(m.Close)
		if !allow {
			// Cold capacity refusals now retain their queued worker. Cancel
			// after its first check to prove the undelivered clone parks Warm
			// without a boot failure or leaked admission/clone slot.
			adm.onCheck = m.workerCancel
		}
		m.runClone("p", store.PoolKindHot, true, nil, func() {})
		require.Equal(t, 1, adm.calls)
		require.Zero(t, adm.reserveCalls)
		rows, err := st.List()
		require.NoError(t, err)
		require.Len(t, rows, 1)
		if !allow {
			require.Empty(t, prov.starts)
			require.Equal(t, store.StateWarm, rows[0].State)
			require.Zero(t, rows[0].BootAttempts, "capacity refusal is not a boot failure")
			continue
		}
		require.Len(t, prov.starts, 1)
		require.Equal(t, store.StateHot, rows[0].State)
		require.Equal(t, uint64(22*gib), adm.snapshotFree("pve1"))
	}
}

func TestPacedLinkedWarmCloneDoesNotHoldLaunchCapacity(t *testing.T) {
	st := newTestStore(t)
	adm := &pacedAdmitterStub{fakeAdmitter: newFakeAdmitter(map[string]uint64{"pve1": gib})}
	prov := &fakeProv{}
	prov.onClone = func(opts provisioner.CloneOptions) {
		require.True(t, opts.Linked)
		require.False(t, opts.PoweredOn)
		require.Equal(t, uint64(gib), adm.snapshotFree("pve1"))
		require.Zero(t, adm.calls)
	}
	m := newTestManager(t, st, prov, Config{
		Profiles: []ProfileSettings{{Name: "p", MemoryMB: 4096, MaxConcurrentRunners: 10}},
		Capacity: adm, LinkedClones: true, WindowsBootBudget: NewHostBootBudget(2, time.Millisecond, 0),
	})
	t.Cleanup(m.Close)

	m.runClone("p", store.PoolKindWarm, false, nil, func() {})
	rows, err := st.List()
	require.NoError(t, err)
	require.Len(t, rows, 1, "a stopped linked clone needs no guest memory headroom")
	require.Equal(t, store.StateWarm, rows[0].State)
	require.Zero(t, adm.reserveCalls)
	require.Empty(t, prov.starts)

	// Reproduce the row transition performed before a promotion worker starts.
	deadline := time.Now().Add(time.Minute)
	booting, err := st.Update(rows[0].VMID, func(row *store.VM) {
		row.State = store.StateBooting
		row.StartWorkerDeadline = deadline
	})
	require.NoError(t, err)
	m.runBoot(context.Background(), booting, deadline, capacityClaim{release: func() {}})
	require.Equal(t, 1, adm.calls, "power-on still consults the fresh paced gate")
	require.Empty(t, prov.starts, "insufficient RAM must prevent the power-on")
	warm, err := st.Get(booting.VMID)
	require.NoError(t, err)
	require.Equal(t, store.StateWarm, warm.State)
}

func TestReturnUndeliveredJITOnlyRequeuesExactUnclaimedAssignment(t *testing.T) {
	st := newTestStore(t)
	m := newTestManager(t, st, &fakeProv{}, Config{})
	t.Cleanup(m.Close)
	at := time.Now()
	vm := &VM{VMID: 10001, Node: "pve1", Name: "runner", Profile: "p", CreatedAt: at.Add(-time.Minute), AssignedAt: at}
	require.NoError(t, st.Insert(&store.VM{
		VMID: vm.VMID, Node: vm.Node, Name: vm.Name, Profile: vm.Profile,
		CreatedAt: vm.CreatedAt, StateSince: at, JITAssignedAt: at,
		State: store.StateAssigned, PoolKind: store.PoolKindHot, RunnerID: 99, JITPending: true,
	}))
	require.False(t, m.ReturnUndeliveredJIT(vm, 100), "wrong runner ID")
	stale := *vm
	stale.CreatedAt = stale.CreatedAt.Add(-time.Second)
	require.False(t, m.ReturnUndeliveredJIT(&stale, 99), "different VM generation")
	stale = *vm
	stale.AssignedAt = stale.AssignedAt.Add(-time.Second)
	require.False(t, m.ReturnUndeliveredJIT(&stale, 99), "different assignment")
	_, err := st.Update(vm.VMID, func(v *store.VM) { v.JobID = 10 })
	require.NoError(t, err)
	require.False(t, m.ReturnUndeliveredJIT(vm, 99), "job already matched")
	_, err = st.Update(vm.VMID, func(v *store.VM) { v.JobID = 0 })
	require.NoError(t, err)
	require.True(t, m.ReturnUndeliveredJIT(vm, 99))
	rows, err := st.List()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, store.StateHot, rows[0].State)
	require.Zero(t, rows[0].RunnerID)
	require.False(t, rows[0].JITPending)
	require.False(t, m.ReturnUndeliveredJIT(vm, 99), "old assignment cannot be reused")
}

func TestPacedHTTPSubmissionsCannotOverlapOrBunch(t *testing.T) {
	const spacing = 20 * time.Millisecond
	b := NewHostBootBudget(2, spacing, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	inFirst := make(chan struct{})
	finishFirst := make(chan struct{})
	firstResponse := make(chan time.Time, 1)
	secondSubmit := make(chan time.Time, 1)
	done := make(chan error, 2)
	go func() {
		done <- b.paceAction(ctx, "pve1", func(mark func()) error {
			mark()
			close(inFirst)
			<-finishFirst
			firstResponse <- time.Now()
			return context.DeadlineExceeded // ambiguous response still paces.
		})
	}()
	<-inFirst
	go func() {
		done <- b.paceAction(ctx, "pve1", func(mark func()) error {
			mark()
			secondSubmit <- time.Now()
			return nil
		})
	}()
	select {
	case <-secondSubmit:
		t.Fatal("second POST entered while the first was in flight")
	case <-time.After(2 * spacing):
	}
	close(finishFirst)
	require.GreaterOrEqual(t, (<-secondSubmit).Sub(<-firstResponse), spacing)
	require.ErrorIs(t, <-done, context.DeadlineExceeded)
	require.NoError(t, <-done)
}
