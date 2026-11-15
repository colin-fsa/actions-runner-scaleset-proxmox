package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/store"
)

// Warm-to-hot promotion starts a powered-off VM, which spends host memory
// exactly like a clone. These tests pin that it passes the same capacity
// gate, and that its claim is rolled back on every failure path.

func promotionManager(t *testing.T, adm *fakeAdmitter, prov *fakeProv) (*manager, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	m := newTestManager(t, st, prov, Config{
		Capacity: adm,
		Profiles: []ProfileSettings{
			{Name: "linux", MemoryMB: 10240, CPUCores: 6, MaxConcurrentRunners: 25},
			{Name: "win", MemoryMB: 4096, CPUCores: 4, MaxConcurrentRunners: 25},
		},
	})
	return m, st
}

func seedWarmRow(t *testing.T, st *store.Store, vmid int, profile string) {
	t.Helper()
	require.NoError(t, st.Insert(&store.VM{
		VMID: vmid, Node: "pve1", Name: "gh-runner-test-warm", Profile: profile,
		State: store.StateWarm, PoolKind: store.PoolKindWarm, CreatedAt: time.Now().Add(-time.Minute),
	}))
}

func stateOf(t *testing.T, st *store.Store, vmid int) store.State {
	t.Helper()
	row, err := st.Get(vmid)
	require.NoError(t, err)
	return row.State
}

// TestPromote_AdmittedOnlyWithinCapacity: 25 GiB free admits two 10 GiB
// Linux promotions, not three; the third stays Warm (nothing to roll
// back) and counts as deferred hot demand.
func TestPromote_AdmittedOnlyWithinCapacity(t *testing.T) {
	t.Parallel()
	adm := newFakeAdmitter(map[string]uint64{"pve1": 25 * gib})
	prov := &fakeProv{}
	m, st := promotionManager(t, adm, prov)
	for _, id := range []int{10001, 10002, 10003} {
		seedWarmRow(t, st, id, "linux")
	}

	m.promoteN(context.Background(), "linux", 3)
	m.wg.Wait()

	require.Equal(t, 3, adm.existingCalls, "every promotion asks the accountant")
	require.Equal(t, uint64(5*gib), adm.snapshotFree("pve1"), "two promotions hold 20 GiB")
	require.Len(t, prov.starts, 2, "only admitted VMs are started")
	require.Equal(t, store.StateWarm, stateOf(t, st, 10003), "the refused VM stays warm")
	require.True(t, m.profileOf("linux").capacityDeferredPromote.Load(),
		"a refused promotion is recorded as unmet hot demand")
	require.Equal(t, float64(1), testutil.ToFloat64(
		m.metrics.CloneDeferredCapacity.WithLabelValues("test", "linux", "pve1", "hot")))
}

// TestPromote_SharedAcrossProfiles: one ledger for every profile, so a
// Windows promotion can use only what Linux has not already claimed.
func TestPromote_SharedAcrossProfiles(t *testing.T) {
	t.Parallel()
	adm := newFakeAdmitter(map[string]uint64{"pve1": 14 * gib})
	m, st := promotionManager(t, adm, &fakeProv{})
	seedWarmRow(t, st, 10001, "linux")
	seedWarmRow(t, st, 10002, "win")
	seedWarmRow(t, st, 10003, "win")

	m.promoteN(context.Background(), "linux", 1)
	m.promoteN(context.Background(), "win", 2)
	m.wg.Wait()

	require.Equal(t, uint64(0), adm.snapshotFree("pve1"), "10 GiB Linux + one 4 GiB Windows")
	require.Equal(t, store.StateWarm, stateOf(t, st, 10003), "the second Windows VM did not fit")
}

// TestPromote_StartFailureReleasesClaim: a VM that never started never
// consumed its memory, so the claim goes straight back.
func TestPromote_StartFailureReleasesClaim(t *testing.T) {
	t.Parallel()
	adm := newFakeAdmitter(map[string]uint64{"pve1": 16 * gib})
	prov := &fakeProv{startErr: errors.New("qmstart failed")}
	m, st := promotionManager(t, adm, prov)
	seedWarmRow(t, st, 10001, "linux")

	m.promoteN(context.Background(), "linux", 1)
	m.wg.Wait()

	require.Equal(t, uint64(16*gib), adm.snapshotFree("pve1"), "a failed start hands its capacity back")
	require.Equal(t, 1, adm.released)
}

// TestPromote_SuccessKeepsClaim: a started VM keeps its claim until the
// accountant observes it powered on, as a successful clone does.
func TestPromote_SuccessKeepsClaim(t *testing.T) {
	t.Parallel()
	adm := newFakeAdmitter(map[string]uint64{"pve1": 16 * gib})
	m, st := promotionManager(t, adm, &fakeProv{})
	seedWarmRow(t, st, 10001, "linux")

	m.promoteN(context.Background(), "linux", 1)
	m.wg.Wait()

	require.Equal(t, uint64(6*gib), adm.snapshotFree("pve1"))
	require.Zero(t, adm.released)
	require.Equal(t, 10001, adm.lastExisting, "the claim is bound to the promoted VM")
}

// TestPromote_LostCASReleasesClaim: if the row leaves Warm between the
// list and the CAS (e.g. a concurrent shrink), promoteN hands the claim
// back and does not start the VM.
func TestPromote_LostCASReleasesClaim(t *testing.T) {
	t.Parallel()
	adm := newFakeAdmitter(map[string]uint64{"pve1": 16 * gib})
	prov := &fakeProv{}
	m, st := promotionManager(t, adm, prov)
	seedWarmRow(t, st, 10001, "linux")
	adm.onReserveExisting = func(vmid int) {
		_, err := st.Update(vmid, func(v *store.VM) { v.State = store.StateDraining })
		require.NoError(t, err)
	}

	m.promoteN(context.Background(), "linux", 1)
	m.wg.Wait()

	require.Empty(t, prov.starts, "a lost CAS must not start the VM")
	require.Equal(t, uint64(16*gib), adm.snapshotFree("pve1"), "and must release its claim")
	require.Equal(t, 1, adm.released)
}

// TestPromote_RefusalDoesNotArmEviction: eviction would destroy this
// profile's own stopped warm VMs, which free no host memory; a refused
// promotion only holds warm refill.
func TestPromote_RefusalDoesNotArmEviction(t *testing.T) {
	t.Parallel()
	adm := newFakeAdmitter(map[string]uint64{"pve1": 4 * gib})
	m, st := promotionManager(t, adm, &fakeProv{})
	seedWarmRow(t, st, 10001, "linux")

	m.promoteN(context.Background(), "linux", 1)
	m.wg.Wait()

	ps := m.profileOf("linux")
	require.False(t, ps.capacityDeferredHot.Load(), "must not arm idle eviction")
	require.True(t, ps.capacityDeferredPromote.Load(), "must hold warm refill")
}

// TestPromote_UnknownCapacityDefers: no snapshot means no promise; the
// VM stays warm rather than start blind.
func TestPromote_UnknownCapacityDefers(t *testing.T) {
	t.Parallel()
	adm := newFakeAdmitter(map[string]uint64{"pve1": 64 * gib})
	adm.err = errors.New("proxmox unreachable")
	prov := &fakeProv{}
	m, st := promotionManager(t, adm, prov)
	seedWarmRow(t, st, 10001, "linux")

	m.promoteN(context.Background(), "linux", 1)
	m.wg.Wait()

	require.Empty(t, prov.starts)
	require.Equal(t, store.StateWarm, stateOf(t, st, 10001))
}

// TestPromote_DisabledAdmissionUnchanged: with no admitter wired the
// promotion path behaves exactly as before.
func TestPromote_DisabledAdmissionUnchanged(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	prov := &fakeProv{}
	m := newTestManager(t, st, prov, Config{})
	seedWarmRow(t, st, 10001, "")

	m.promoteN(context.Background(), "", 1)
	m.wg.Wait()
	require.Equal(t, []int{10001}, prov.starts)
}

// TestClone_WarmClaimRetiresOnObservation: a powered-off warm clone keeps
// its claim (no early release — that would undercount until the snapshot
// shows the row) but marks it to retire on observation rather than on a
// power-on that never comes.
func TestClone_WarmClaimRetiresOnObservation(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	adm := newFakeAdmitter(map[string]uint64{"pve1": 32 * gib})
	m := newTestManager(t, st, &fakeProv{}, Config{
		Capacity: adm,
		Profiles: []ProfileSettings{{Name: "p", MemoryMB: 8192, MaxConcurrentRunners: 10}},
	})

	m.runClone("p", store.PoolKindWarm, false, nil, func() {})

	require.Equal(t, 1, adm.expectedOff, "the warm clone's claim is marked powered-off")
	require.Zero(t, adm.released, "and is not released early")
}

// TestClone_HotClaimNotMarkedPoweredOff: a powered-on clone's claim keeps
// the normal retire-on-power rule.
func TestClone_HotClaimNotMarkedPoweredOff(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	adm := newFakeAdmitter(map[string]uint64{"pve1": 32 * gib})
	m := newTestManager(t, st, &fakeProv{}, Config{
		Capacity: adm,
		Profiles: []ProfileSettings{{Name: "p", MemoryMB: 8192, MaxConcurrentRunners: 10}},
	})

	m.runClone("p", store.PoolKindHot, true, nil, func() {})

	require.Zero(t, adm.expectedOff)
	require.Zero(t, adm.released)
}

// TestReconcile_WarmRefillHeldWhileHotDeferred: warm refill must not
// compete for memory with hot demand the gate refused — a hot clone in
// the previous pass, or a promotion in this one — and resumes on the next
// pass once nothing was refused.
func TestReconcile_WarmRefillHeldWhileHotDeferred(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		arm  func(ps *profileState)
	}{
		{"hot clone refused last pass", func(ps *profileState) { ps.capacityDeferredHot.Store(true) }},
		{"promotion refused this pass", func(ps *profileState) { ps.capacityDeferredPromote.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := newTestStore(t)
			prov := &fakeProv{}
			adm := newFakeAdmitter(map[string]uint64{"pve1": 64 * gib})
			m := newTestManager(t, st, prov, Config{
				Capacity: adm,
				Profiles: []ProfileSettings{{Name: "p", MemoryMB: 4096, MaxConcurrentRunners: 10, HotSize: 0, WarmSize: 3}},
			})
			tc.arm(m.profileOf("p"))

			m.reconcileProfileOnce(context.Background(), "p")
			m.wg.Wait()
			prov.mu.Lock()
			held := len(prov.clones)
			prov.mu.Unlock()
			require.Zero(t, held, "no warm refill in the pass that saw the refusal")

			// Nothing armed the flags again: the next pass refills.
			m.reconcileProfileOnce(context.Background(), "p")
			require.Eventually(t, func() bool {
				prov.mu.Lock()
				defer prov.mu.Unlock()
				return len(prov.clones) == 3
			}, 5*time.Second, 10*time.Millisecond, "refill resumes once nothing is refused")
		})
	}
}
