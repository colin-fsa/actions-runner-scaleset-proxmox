package pool

import (
	"context"
	"testing"
	"time"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/store"
	"github.com/stretchr/testify/require"
)

func TestRecoveryOnlyBlocksWorkRequiringNewPower(t *testing.T) {
	prov := &fakeProv{powerStateBy: map[int]string{12003: "stopped"}}
	gate := NewHostBootBudget(2, time.Nanosecond, 0)
	gate.RequireRecovery(true)
	st := newTestStore(t)
	registered := map[string]RunnerInfo{"win-completed": {ID: 17, Busy: true, Online: true}}
	m := newTestManager(t, st, prov, Config{
		Profiles:          []ProfileSettings{{Name: "linux", GuestType: config.GuestTypeLXC, MemoryMB: 10240, HotSize: 1, WarmSize: 1}, {Name: "windows", GuestOS: "windows", GuestType: config.GuestTypeQEMU, MemoryMB: 4096, HotSize: 1, WarmSize: 1}},
		WindowsBootBudget: gate, RunnerLister: func(context.Context) (map[string]RunnerInfo, error) { return registered, nil },
	})
	t.Cleanup(m.Close)
	now := time.Now()
	for _, r := range []*store.VM{
		{VMID: 11001, Node: "pve1", Profile: "linux", Name: "linux-hot", State: store.StateHot, PoolKind: store.PoolKindHot, CreatedAt: now.Add(-time.Minute)},
		{VMID: 12002, Node: "pve1", Profile: "windows", Name: "win-pristine", State: store.StateHot, PoolKind: store.PoolKindHot, CreatedAt: now},
		{VMID: 12003, Node: "pve1", Profile: "windows", Name: "win-completed", State: store.StateAssigned, PoolKind: store.PoolKindHot, AdoptionProtected: true, RestartCleanupPending: true},
	} {
		require.NoError(t, st.Insert(r))
	}
	m.reconcileProfileOnce(t.Context(), "linux")
	m.reconcileProfileOnce(t.Context(), "windows")
	require.False(t, prov.clonedProfile("linux"))
	require.False(t, prov.clonedProfile("windows"))
	_, err := m.AcquireForProfile(t.Context(), 0, "linux", 0)
	require.ErrorIs(t, err, ErrNoneAvailable, "do not consume a runner whose JIT would reboot the host guest")
	v, err := m.Acquire(t.Context(), 0, 0)
	require.NoError(t, err)
	require.Equal(t, 12002, v.VMID, "already ready Windows can receive JIT without a power operation")
	rows, err := m.ListRows(t.Context())
	require.NoError(t, err)
	var stopped RowSnapshot
	for _, r := range rows {
		if r.VMID == 12003 {
			stopped = r
		}
	}
	require.ErrorContains(t, m.QueueObservedRestartCleanup(t.Context(), stopped), "busy")
	prov.mu.Lock()
	require.Empty(t, prov.destroys)
	prov.mu.Unlock()
	registered = map[string]RunnerInfo{}
	entered := make(chan struct{})
	prov.mu.Lock()
	prov.destroyEntered = entered
	prov.mu.Unlock()
	require.NoError(t, m.QueueObservedRestartCleanup(t.Context(), stopped))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("verified stopped cleanup never reached ordinary destroy")
	}
	require.True(t, gate.RecoveryPending(), "cleanup did not authorize any starts")
}

func TestLateRestartGuestIsInsertedWithoutPowerOrCleanup(t *testing.T) {
	const vmid = 12003
	prov := &fakeProv{powerStateBy: map[int]string{vmid: "stopped"}}
	gate := NewHostBootBudget(2, time.Nanosecond, 0)
	gate.RequireRecovery(true)
	m := newTestManager(t, newTestStore(t), prov, Config{
		VMNamePrefix: "owner-", Profiles: []ProfileSettings{{Name: "windows", GuestOS: "windows", GuestType: config.GuestTypeQEMU, MemoryMB: 4096}},
		WindowsBootBudget: gate, RunnerLister: func(context.Context) (map[string]RunnerInfo, error) { return map[string]RunnerInfo{}, nil },
	})
	t.Cleanup(m.Close)
	vm := &provisioner.VM{VMID: vmid, Node: "pve1", Name: "owner-12003", Profile: "windows", GuestType: "qemu"}
	require.NoError(t, m.AdoptLateRestartGuest(t.Context(), vm))
	require.NoError(t, m.AdoptLateRestartGuest(t.Context(), vm))
	rows, err := m.ListRows(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, vm.Name, rows[0].Name)
	require.True(t, rows[0].RestartCleanupPending)
	require.True(t, rows[0].AdoptionProtected)
	prov.mu.Lock()
	defer prov.mu.Unlock()
	require.Empty(t, prov.destroys)
	require.Empty(t, prov.starts)
}

func TestPretagQuarantineDoesNotBecomeAJobOrDestroyOnEvents(t *testing.T) {
	const vmid = 12003
	gate := NewHostBootBudget(2, time.Nanosecond, 0)
	gate.RequireRecovery(true)
	prov := &fakeProv{powerStateBy: map[int]string{vmid: "stopped"}}
	m := newTestManager(t, newTestStore(t), prov, Config{VMNamePrefix: "owner-", WindowsBootBudget: gate, MaxConcurrentRunners: 1, VMIDRange: config.VMIDRange{Min: 12003, Max: 12005},
		Profiles:     []ProfileSettings{{Name: "windows", GuestOS: "windows", GuestType: config.GuestTypeQEMU, MemoryMB: 4096}},
		RunnerLister: func(context.Context) (map[string]RunnerInfo, error) { return map[string]RunnerInfo{}, nil }})
	t.Cleanup(m.Close)
	vm := &provisioner.VM{VMID: vmid, Node: "pve1", Name: "owner-12003", Profile: "windows", GuestType: "qemu"}
	require.NoError(t, m.QuarantinePretagRestartGuest(t.Context(), vm, ""))
	require.NoError(t, m.MarkRunning(t.Context(), vmid, 1))
	require.NoError(t, m.MarkCompleted(t.Context(), vmid))
	rows, err := m.ListRows(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.True(t, rows[0].RestartPretagQuarantine)
	require.Equal(t, "excluded", rows[0].State)
	require.NoError(t, m.ForceDestroy(t.Context(), vmid, "must not touch excluded ID"))
	require.NoError(t, m.store.Insert(&store.VM{VMID: 12004, Node: "pve1", Name: "owner-12004", Profile: "windows", State: store.StateHot, PoolKind: store.PoolKindHot}))
	next, err := m.allocateVMID(t.Context())
	require.NoError(t, err)
	require.Equal(t, 12005, next, "excluded ID still occupies allocator namespace")
	gate.RequireRecovery(false)
	claimed, err := m.Acquire(t.Context(), 0, 0)
	require.NoError(t, err, "excluded ID does not count against max concurrent runners of one")
	require.Equal(t, 12004, claimed.VMID)
	prov.mu.Lock()
	defer prov.mu.Unlock()
	require.Empty(t, prov.destroys)
	require.Empty(t, prov.starts)
}
