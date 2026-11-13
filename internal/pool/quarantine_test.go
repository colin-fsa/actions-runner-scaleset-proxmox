package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/store"
)

type quarantineFake struct {
	*fakeProv
	err      error
	calls    int
	free     bool
	checkErr error
}

func (f *quarantineFake) Quarantine(_ context.Context, _ *provisioner.VM, _ time.Time) error {
	f.calls++
	return f.err
}

func (f *quarantineFake) QuarantineVMIDFree(_ context.Context, _ int) (bool, error) {
	return f.free, f.checkErr
}

func TestQuarantineProtectionFailureReservesAndRetries(t *testing.T) {
	st, err := store.New()
	require.NoError(t, err)
	require.NoError(t, st.Insert(&store.VM{VMID: 10001, Node: "pve1", Name: "runner", Profile: defaultProfileName, PoolKind: store.PoolKindHot, State: store.StateAssigned}))
	f := &quarantineFake{fakeProv: &fakeProv{}, err: errors.New("pve unavailable")}
	m := newTestManager(t, st, f, Config{})
	require.True(t, m.quarantineCandidate(context.Background(), 10001))
	row, err := st.Get(10001)
	require.NoError(t, err)
	require.Equal(t, store.StateQuarantined, row.State)
	require.ErrorIs(t, m.ForceDestroy(context.Background(), 10001, "test"), ErrQuarantineHeld)
	row, err = st.Get(10001)
	require.NoError(t, err)
	require.Equal(t, store.StateQuarantined, row.State)
	f.err = nil
	m.retryQuarantineProtection(context.Background())
	require.Equal(t, 2, f.calls)
	require.Empty(t, m.pendingProtection)
}

func TestPendingProtectionSurvivesManagerRestart(t *testing.T) {
	dir := t.TempDir()
	st := newTestStore(t)
	require.NoError(t, st.Insert(&store.VM{VMID: 10001, Node: "pve1", Name: "runner", Profile: defaultProfileName, PoolKind: store.PoolKindHot, State: store.StateAssigned}))
	f := &quarantineFake{fakeProv: &fakeProv{}, err: errors.New("pve unavailable")}
	cfg := Config{QuarantineSuspiciousWindows: true, QuarantineJournalDir: dir}
	m := newTestManager(t, st, f, cfg)
	require.True(t, m.quarantineCandidate(context.Background(), 10001))
	originalDeadline := m.pendingProtection[10001]
	m.Close()

	recovered := newTestStore(t)
	f.err = nil
	m2 := newTestManager(t, recovered, f, cfg)
	defer m2.Close()
	row, err := recovered.Get(10001)
	require.NoError(t, err)
	require.Equal(t, store.StateQuarantined, row.State)
	require.True(t, originalDeadline.Equal(m2.pendingProtection[10001]))
	// An unmarked stopped VM must not be reclassified as disposable on adopt.
	m2.adoptOne(context.Background(), &provisioner.VM{VMID: 10001, Node: "pve1", Name: "runner", Profile: defaultProfileName}, nil)
	row, err = recovered.Get(10001)
	require.NoError(t, err)
	require.Equal(t, store.StateQuarantined, row.State)
	m2.retryQuarantineProtection(context.Background())
	require.Empty(t, m2.pendingProtection)
}

func TestQuarantineJournalRequiresDurableDirectory(t *testing.T) {
	_, err := newQuarantineJournal("", "test")
	require.Error(t, err)
	_, err = newQuarantineJournal("relative", "test")
	require.Error(t, err)
	_, err = newQuarantineJournal(t.TempDir()+"/missing", "test")
	require.Error(t, err)
}

func TestQuarantineKeepsSingleSlot(t *testing.T) {
	st, err := store.New()
	require.NoError(t, err)
	for _, id := range []int{10001, 10002} {
		require.NoError(t, st.Insert(&store.VM{VMID: id, Node: "pve1", Name: "runner", Profile: defaultProfileName, PoolKind: store.PoolKindHot, State: store.StateAssigned}))
	}
	f := &quarantineFake{fakeProv: &fakeProv{}}
	m := newTestManager(t, st, f, Config{})
	require.True(t, m.quarantineCandidate(context.Background(), 10001))
	require.False(t, m.quarantineCandidate(context.Background(), 10002))
	require.Equal(t, 1, f.calls)
	row, err := st.Get(10002)
	require.NoError(t, err)
	require.Equal(t, store.StateAssigned, row.State)
}

func TestPowerPollQuarantinesStoppedAssignedWindows(t *testing.T) {
	st := newTestStore(t)
	require.NoError(t, st.Insert(&store.VM{VMID: 10001, Node: "pve1", Name: "runner", Profile: "windows", PoolKind: store.PoolKindHot, State: store.StateAssigned}))
	f := &quarantineFake{fakeProv: &fakeProv{powerStateBy: map[int]string{10001: "stopped"}}}
	m := newTestManager(t, st, f, Config{QuarantineSuspiciousWindows: true, QuarantineJournalDir: t.TempDir(), Profiles: []ProfileSettings{{Name: "windows", GuestOS: "windows"}}})
	m.powerPollOnce(context.Background())
	row, err := st.Get(10001)
	require.NoError(t, err)
	require.Equal(t, store.StateQuarantined, row.State)
	require.Equal(t, 1, f.calls)
}

func TestAdoptPreservesMarkedQuarantineEvenIfFeatureDisabled(t *testing.T) {
	st := newTestStore(t)
	f := &fakeProv{powerStateBy: map[int]string{10001: "stopped"}}
	m := newTestManager(t, st, f, Config{})
	m.adoptOne(context.Background(), &provisioner.VM{VMID: 10001, Node: "pve1", Name: "runner", Profile: defaultProfileName, Quarantined: true}, nil)
	row, err := st.Get(10001)
	require.NoError(t, err)
	require.Equal(t, store.StateQuarantined, row.State)
}

func TestDisabledQuarantineKeepsExistingCompletionBehavior(t *testing.T) {
	st := newTestStore(t)
	require.NoError(t, st.Insert(&store.VM{VMID: 10001, Node: "pve1", Name: "runner", Profile: "windows", PoolKind: store.PoolKindHot, State: store.StateAssigned}))
	f := &quarantineFake{fakeProv: &fakeProv{powerStateBy: map[int]string{10001: "stopped"}}}
	m := newTestManager(t, st, f, Config{Profiles: []ProfileSettings{{Name: "windows", GuestOS: "windows"}}})
	m.powerPollOnce(context.Background())
	require.Zero(t, f.calls)
	row, err := st.Get(10001)
	if err == nil {
		require.NotEqual(t, store.StateQuarantined, row.State)
	}
}

func TestForceDestroyQuarantinesStoppedAssignedWindows(t *testing.T) {
	st := newTestStore(t)
	require.NoError(t, st.Insert(&store.VM{VMID: 10001, Node: "pve1", Name: "runner", Profile: "windows", PoolKind: store.PoolKindHot, State: store.StateAssigned}))
	f := &quarantineFake{fakeProv: &fakeProv{powerStateBy: map[int]string{10001: "stopped"}}}
	m := newTestManager(t, st, f, Config{QuarantineSuspiciousWindows: true, QuarantineJournalDir: t.TempDir(), Profiles: []ProfileSettings{{Name: "windows", GuestOS: "windows"}}})
	require.ErrorIs(t, m.ForceDestroy(context.Background(), 10001, "assigned: runner never registered on GitHub"), ErrQuarantineHeld)
	row, err := st.Get(10001)
	require.NoError(t, err)
	require.Equal(t, store.StateQuarantined, row.State)
	require.Equal(t, 1, f.calls)
}

func TestForceDestroyRetainsRunningUnregisteredWindowsWithoutStopping(t *testing.T) {
	st := newTestStore(t)
	require.NoError(t, st.Insert(&store.VM{VMID: 10001, Node: "pve1", Name: "runner", Profile: "windows", PoolKind: store.PoolKindHot, State: store.StateAssigned}))
	f := &quarantineFake{fakeProv: &fakeProv{powerStateBy: map[int]string{10001: "running"}}}
	m := newTestManager(t, st, f, Config{QuarantineSuspiciousWindows: true, QuarantineJournalDir: t.TempDir(), Profiles: []ProfileSettings{{Name: "windows", GuestOS: "windows"}}})
	defer m.Close()
	require.ErrorIs(t, m.ForceDestroy(context.Background(), 10001, "assigned: runner never registered on GitHub"), ErrQuarantineHeld)
	row, err := st.Get(10001)
	require.NoError(t, err)
	require.Equal(t, store.StateQuarantined, row.State)
	require.Equal(t, 1, f.calls)
	require.Empty(t, f.destroys)
}

func TestForceDestroyDefersWhenCandidatePowerStateUnknown(t *testing.T) {
	st := newTestStore(t)
	require.NoError(t, st.Insert(&store.VM{VMID: 10001, Node: "pve1", Name: "runner", Profile: "windows", PoolKind: store.PoolKindHot, State: store.StateAssigned}))
	f := &quarantineFake{fakeProv: &fakeProv{powerStateErrBy: map[int]error{10001: errors.New("unreachable")}}}
	m := newTestManager(t, st, f, Config{QuarantineSuspiciousWindows: true, QuarantineJournalDir: t.TempDir(), Profiles: []ProfileSettings{{Name: "windows", GuestOS: "windows"}}})
	require.ErrorContains(t, m.ForceDestroy(context.Background(), 10001, "assigned: runner never registered on GitHub"), "cannot safely inspect")
	row, err := st.Get(10001)
	require.NoError(t, err)
	require.Equal(t, store.StateAssigned, row.State)
	require.Zero(t, f.calls)
}

func TestForgetQuarantineRequiresConfirmedAbsentVM(t *testing.T) {
	st := newTestStore(t)
	require.NoError(t, st.Insert(&store.VM{VMID: 10001, Node: "pve1", Name: "runner", Profile: defaultProfileName, PoolKind: store.PoolKindHot, State: store.StateAssigned}))
	f := &quarantineFake{fakeProv: &fakeProv{}}
	m := newTestManager(t, st, f, Config{QuarantineSuspiciousWindows: true, QuarantineJournalDir: t.TempDir()})
	defer m.Close()
	require.True(t, m.quarantineCandidate(context.Background(), 10001))
	require.ErrorIs(t, m.ForgetQuarantine(context.Background(), 10001), ErrQuarantineVMExists)
	_, err := st.Get(10001)
	require.NoError(t, err)
	f.checkErr = errors.New("cluster unavailable")
	require.Error(t, m.ForgetQuarantine(context.Background(), 10001))
	f.checkErr = nil
	f.free = true
	require.NoError(t, m.ForgetQuarantine(context.Background(), 10001))
	_, err = st.Get(10001)
	require.ErrorIs(t, err, store.ErrNotFound)
	rec, err := m.quarantineJournal.load()
	require.NoError(t, err)
	require.Nil(t, rec)
}

func TestOverdueQuarantineWarnsWithoutDeleting(t *testing.T) {
	st := newTestStore(t)
	require.NoError(t, st.Insert(&store.VM{VMID: 10001, Node: "pve1", Name: "runner", Profile: defaultProfileName, PoolKind: store.PoolKindHot, State: store.StateQuarantined}))
	f := &quarantineFake{fakeProv: &fakeProv{}}
	m := newTestManager(t, st, f, Config{})
	defer m.Close()
	m.quarantineReview = map[int]time.Time{10001: time.Now().Add(-time.Minute)}
	m.warnOverdueQuarantines()
	require.False(t, m.quarantineReviewLastLog[10001].IsZero())
	row, err := st.Get(10001)
	require.NoError(t, err)
	require.Equal(t, store.StateQuarantined, row.State)
	require.Empty(t, f.destroys)
}

func TestQuarantineDoesNotCountAsUsefulRunningWork(t *testing.T) {
	stats := Stats{Quarantined: 1}
	require.Equal(t, 0, stats.Busy())
	require.Equal(t, 1, stats.Total())
}
