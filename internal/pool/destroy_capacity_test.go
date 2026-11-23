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
