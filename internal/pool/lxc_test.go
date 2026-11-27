package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/store"
)

// A hot LXC container whose env already carries a JIT config is serving
// a job: adoption must not leave it Hot, or the scaler could hand it a
// second JIT and reboot it mid-job.
func TestAdoptLXCJITReadback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		jit   []byte
		err   error
		state store.State
	}{
		{"clean", []byte{}, nil, store.StateHot},
		{"consumed", []byte("jit"), nil, store.StateAssigned},
		{"unreadable", nil, errors.New("config unavailable"), store.StateAssigned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			fp := &adoptWindowsProv{fakeProv: &fakeProv{}, jit: tc.jit, readErr: tc.err}
			mgr := newTestManager(t, st, fp, Config{Profiles: []ProfileSettings{{Name: "ct", GuestType: config.GuestTypeLXC, MaxConcurrentRunners: 10}}})
			mgr.adoptOne(context.Background(), &provisioner.VM{VMID: 10001, Node: "pve1", Name: "runner", Profile: "ct", GuestType: config.GuestTypeLXC}, map[string]RunnerInfo{})
			row, err := st.Get(10001)
			require.NoError(t, err)
			require.Equal(t, tc.state, row.State)
		})
	}
}

// A stopped LXC container is reused (adopted Warm) only when its env
// carries no JIT config. One that holds a JIT already ran or was handed a
// job and powered off before the power poller destroyed it (crash or
// leader handover in between): its JIT is consumed and its filesystem
// dirty, so it must be destroyed, never promoted. An unreadable config
// fails safe the same way.
func TestAdoptStoppedLXC(t *testing.T) {
	for _, tc := range []struct {
		name    string
		jit     []byte
		err     error
		destroy bool
	}{
		{"clean", []byte{}, nil, false},
		// ReadJITConfig returns only the ACTIONS_RUNNER_JIT_CONFIG entry,
		// so a template-inherited env such as NEXT_TELEMETRY_DISABLED=1
		// reads back empty and the container is reused.
		{"unrelated env only", nil, nil, false},
		{"used", []byte("jit"), nil, true},
		{"unreadable", nil, errors.New("config unavailable"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			fp := &adoptWindowsProv{fakeProv: &fakeProv{powerStateBy: map[int]string{10001: "stopped"}}, jit: tc.jit, readErr: tc.err}
			mgr := newTestManager(t, st, fp, Config{Profiles: []ProfileSettings{{Name: "ct", GuestType: config.GuestTypeLXC, MaxConcurrentRunners: 10}}})
			mgr.adoptOne(context.Background(), &provisioner.VM{VMID: 10001, Node: "pve1", Name: "runner", Profile: "ct", GuestType: config.GuestTypeLXC}, map[string]RunnerInfo{})
			if !tc.destroy {
				row, err := st.Get(10001)
				require.NoError(t, err)
				require.Equal(t, store.StateWarm, row.State)
				return
			}
			require.Eventually(t, func() bool { fp.mu.Lock(); defer fp.mu.Unlock(); return len(fp.destroys) > 0 }, time.Second, time.Millisecond,
				"a used or unverifiable stopped container must be destroyed")
		})
	}
}

func TestCloneCarriesProfileGuestType(t *testing.T) {
	t.Parallel()
	fp := &fakeProv{}
	mgr := newTestManager(t, newTestStore(t), fp, Config{
		MaxConcurrentRunners: 10,
		Profiles: []ProfileSettings{
			{Name: "ct", GuestType: config.GuestTypeLXC, HotSize: 1, MaxConcurrentRunners: 10, BootMaxAttempts: 3},
		},
	})
	mgr.reconcileProfileOnce(context.Background(), "ct")
	require.Eventually(t, func() bool { return fp.clonedProfile("ct") }, 2*time.Second, 10*time.Millisecond)
	fp.mu.Lock()
	defer fp.mu.Unlock()
	require.Equal(t, config.GuestTypeLXC, fp.clones[0].GuestType)
}

// A freshly demanded Linux container must be assignable before its first boot:
// JIT is a startup env value, so a speculative boot would require a reboot.
func TestLXCColdCloneDefersFirstBootUntilJIT(t *testing.T) {
	for _, guestOS := range []string{"", "linux"} {
		t.Run("os="+guestOS, func(t *testing.T) {
			st := newTestStore(t)
			fp := &fakeProv{waitErr: errors.New("must not poll an unbooted guest")}
			m := newTestManager(t, st, fp, Config{Profiles: []ProfileSettings{{Name: "ct", GuestOS: guestOS, GuestType: config.GuestTypeLXC}}})
			t.Cleanup(m.Close)
			var vmid int
			m.runClone("ct", store.PoolKindHot, true, &vmid, func() {})
			row, err := st.Get(vmid)
			require.NoError(t, err)
			require.Equal(t, store.StateHot, row.State)
			require.Equal(t, store.PoolKindHot, row.PoolKind)
			require.True(t, row.StartWorkerDeadline.IsZero(), "finished clone cannot retain a boot worker")
			fp.mu.Lock()
			require.Len(t, fp.clones, 1)
			require.False(t, fp.clones[0].PoweredOn)
			require.Empty(t, fp.starts, "pool must not boot before scaler generates JIT")
			fp.mu.Unlock()
			vm, err := m.AcquireForProfile(t.Context(), 42, "ct", 0)
			require.NoError(t, err)
			require.Equal(t, vmid, vm.VMID)
			row, err = st.Get(vmid)
			require.NoError(t, err)
			require.True(t, row.JITPending, "power poller must ignore the stopped guest until handoff finishes")
		})
	}
}

func TestQEMUColdCloneStillBootsBeforeAssignment(t *testing.T) {
	for _, guestOS := range []string{"linux", "windows"} {
		t.Run(guestOS, func(t *testing.T) {
			st := newTestStore(t)
			fp := &fakeProv{}
			m := newTestManager(t, st, fp, Config{Profiles: []ProfileSettings{{Name: "vm", GuestOS: guestOS, GuestType: config.GuestTypeQEMU}}})
			t.Cleanup(m.Close)
			var vmid int
			m.runClone("vm", store.PoolKindHot, true, &vmid, func() {})
			row, err := st.Get(vmid)
			require.NoError(t, err)
			require.Equal(t, store.StateHot, row.State)
			fp.mu.Lock()
			defer fp.mu.Unlock()
			require.Equal(t, []int{vmid}, fp.starts)
		})
	}
}
