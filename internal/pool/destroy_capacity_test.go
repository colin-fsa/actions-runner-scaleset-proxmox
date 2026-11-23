package pool

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/store"
)

type destroyCaptureAdmitter struct {
	*fakeAdmitter
	captured, released int
}

func (a *destroyCaptureAdmitter) ReservationsBeforeDestroy(_ string, _ int) func() {
	a.captured++
	return func() { a.released++ }
}

func TestDestroyReleasesReservationsOnlyAfterProvisionerSuccess(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "ambiguous failure"
		}
		t.Run(name, func(t *testing.T) {
			st := newTestStore(t)
			require.NoError(t, st.Insert(&store.VM{
				VMID: 10001, Node: "pve1", Name: "gh-runner-test-10001",
				Profile: defaultProfileName, State: store.StateDestroying, PoolKind: store.PoolKindHot,
			}))
			adm := &destroyCaptureAdmitter{fakeAdmitter: newFakeAdmitter(nil)}
			p := &fakeProv{}
			if fail {
				p.destroyErr = errors.New("destroy result uncertain")
			}
			m := newTestManager(t, st, p, Config{Capacity: adm})
			m.destroy(context.Background(), 10001, "pve1")
			require.Equal(t, 1, adm.captured)
			_, err := st.Get(10001)
			if fail {
				require.Zero(t, adm.released)
				require.NoError(t, err)
			} else {
				require.Equal(t, 1, adm.released)
				require.ErrorIs(t, err, store.ErrNotFound)
			}
		})
	}
}

func TestForgetAbsentAssignedCannotDeleteNewGeneration(t *testing.T) {
	st := newTestStore(t)
	p := &fakeProv{}
	m := newTestManager(t, st, p, Config{})
	v := &store.VM{VMID: 10001, Node: "pve1", Name: "gh-runner-test-10001", Profile: defaultProfileName, State: store.StateAssigned, PoolKind: store.PoolKindHot, AdoptionProtected: true}
	require.NoError(t, st.Insert(v))
	rs, err := m.ListRows(context.Background())
	require.NoError(t, err)
	require.Len(t, rs, 1)
	stale := rs[0]
	_, err = st.Update(v.VMID, func(v *store.VM) { v.RunnerID = 100 })
	require.NoError(t, err)
	removed, err := m.ForgetAbsentAssigned(context.Background(), stale)
	require.NoError(t, err)
	require.False(t, removed)
	_, err = st.Update(v.VMID, func(v *store.VM) { v.RunnerID = 0 })
	require.NoError(t, err)
	rs, err = m.ListRows(context.Background())
	require.NoError(t, err)
	removed, err = m.ForgetAbsentAssigned(context.Background(), rs[0])
	require.NoError(t, err)
	require.True(t, removed)
	_, err = st.Get(v.VMID)
	require.ErrorIs(t, err, store.ErrNotFound)
}
