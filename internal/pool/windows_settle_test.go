package pool

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/store"
)

type onlineSettleSpy struct {
	CapacityAdmitter
	positive int
	revoked  int
}

func (s *onlineSettleSpy) ObserveWindowsOnline(string, string, int, time.Time, time.Time, time.Time) {
	s.positive++
}
func (s *onlineSettleSpy) ForgetWindowsOnline(string, int, time.Time, time.Time) {
	s.revoked++
}

func TestWindowsSettleRequiresExactSuccessfulJITAndRunnerID(t *testing.T) {
	st := newTestStore(t)
	spy := &onlineSettleSpy{}
	m := newTestManager(t, st, &fakeProv{}, Config{
		Capacity: spy, ObserveWindowsOnline: true, Profiles: []ProfileSettings{
			{Name: "windows", GuestOS: "windows", MemoryMB: 4096},
			{Name: "linux", GuestOS: "linux", GuestType: "lxc", MemoryMB: 10240},
		},
	})
	ctx := context.Background()
	for _, profile := range []string{"windows", "linux"} {
		id := 12001
		if profile == "linux" {
			id = 11001
		}
		gen := time.Now().Add(-time.Minute)
		acquired := gen.Add(time.Second)
		vm := &VM{VMID: id, Name: profile, Node: "pve1", Profile: profile, CreatedAt: gen, AssignedAt: acquired}
		require.NoError(t, st.Insert(&store.VM{
			VMID: id, Node: vm.Node, Name: vm.Name, Profile: profile, CreatedAt: gen,
			State: store.StateAssigned, PoolKind: store.PoolKindHot, JITPending: true, JITAssignedAt: acquired, RunnerID: 17,
		}))
		row := RowSnapshot{VMID: id, Node: vm.Node, Name: vm.Name, Profile: profile, CreatedAt: gen, RunnerID: 17}
		m.ObserveWindowsRunner(row, RunnerInfo{ID: 17, Online: true}, time.Now())
		require.Zero(t, spy.positive, "registered runner is not proof of successful JIT delivery")
		wrong := *vm
		wrong.AssignedAt = wrong.AssignedAt.Add(time.Second)
		m.RecordWindowsJITDelivery(&wrong, 17)
		m.RecordWindowsJITDelivery(vm, 99)
		unconfirmed, err := st.Get(id)
		require.NoError(t, err)
		require.True(t, unconfirmed.WindowsJITAt.IsZero())
		m.RecordWindowsJITDelivery(vm, 17)
		m.FinishJITDelivery(id, gen, acquired)
		m.ObserveWindowsRunner(row, RunnerInfo{ID: 17, Online: true}, acquired)
		require.Zero(t, spy.positive, "poll begun before delivery cannot count")
		m.ObserveWindowsRunner(row, RunnerInfo{ID: 19, Online: true}, time.Now())
		require.Zero(t, spy.positive, "a new runner ID cannot inherit the proof")
		m.ObserveWindowsRunner(row, RunnerInfo{ID: 17, Online: false, Busy: true}, time.Now())
		require.Zero(t, spy.positive, "offline even if busy is not success")
		m.ObserveWindowsRunner(row, RunnerInfo{ID: 17, Online: true}, time.Now())
		if profile == "windows" {
			require.Equal(t, 1, spy.positive)
			spy.positive = 0
		} else {
			require.Zero(t, spy.positive)
		}
		_, err = m.ListRows(ctx)
		require.NoError(t, err)
	}
	require.Equal(t, 3, spy.revoked)
}
