package gh

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/pool"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
	"github.com/stretchr/testify/require"
)

func TestOfflineGraceRecoveryAndRestart(t *testing.T) {
	var status atomic.Value
	status.Store("offline")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		busy := "false"
		if status.Load().(string) == "online" {
			busy = "true"
		}
		_, _ = w.Write([]byte(`{"total_count":1,"runners":[{"id":111,"name":"gh-runner-test-2001","status":"` + status.Load().(string) + `","busy":` + busy + `}]}`))
	}))
	defer srv.Close()
	now := time.Now()
	mgr := &fakeManager{rows: []pool.RowSnapshot{{VMID: 2001, Name: "gh-runner-test-2001", State: "running", StateSince: now.Add(-time.Hour)}}}
	makeReconciler := func() *Reconciler {
		r, err := New(baseCfg(), newTestClient(t, srv), mgr, &stubProv{}, silentLogger(), nil)
		require.NoError(t, err)
		r.now = func() time.Time { return now }
		return r
	}
	r := makeReconciler()
	require.NoError(t, r.Tick(context.Background()))
	require.Empty(t, mgr.destroyCalls)
	now = now.Add(r.cfg.RunningIdleGrace / 2)
	status.Store("online")
	require.NoError(t, r.Tick(context.Background()))
	status.Store("offline")
	require.NoError(t, r.Tick(context.Background()))
	now = now.Add(r.cfg.RunningIdleGrace + time.Second)
	r = makeReconciler() // restart cannot inherit the previous observation
	require.NoError(t, r.Tick(context.Background()))
	require.Empty(t, mgr.destroyCalls)
	now = now.Add(r.cfg.RunningIdleGrace + time.Second)
	require.NoError(t, r.Tick(context.Background()))
	require.Len(t, mgr.destroyCalls, 1)
}

func TestPendingJITCannotBeReaped(t *testing.T) {
	srv := runnersServer(t, []fakeRunner{{id: 42, name: "gh-runner-test-42", status: "offline"}})
	defer srv.Close()
	now := time.Now()
	mgr := &fakeManager{rows: []pool.RowSnapshot{{VMID: 42, Name: "gh-runner-test-42", State: "assigned", StateSince: now.Add(-time.Hour), JITPending: true}}}
	r, err := New(baseCfg(), newTestClient(t, srv), mgr, &stubProv{}, silentLogger(), nil)
	require.NoError(t, err)
	r.now = func() time.Time { return now }
	require.NoError(t, r.Tick(context.Background()))
	now = now.Add(time.Hour)
	require.NoError(t, r.Tick(context.Background()))
	require.Empty(t, mgr.destroyCalls)
	mgr.rows[0].JITPending = false
	require.NoError(t, r.Tick(context.Background()))
	require.Empty(t, mgr.destroyCalls)
	now = now.Add(r.cfg.AssignedOfflineGrace + time.Second)
	require.NoError(t, r.Tick(context.Background()))
	require.Len(t, mgr.destroyCalls, 1)
}

func TestOfflineBusyVetoesCleanupIncludingFinalCheck(t *testing.T) {
	var mode atomic.Int32
	var calls atomic.Int32
	mode.Store(1) // busy on every response
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		busy := mode.Load() == 1 || (mode.Load() == 2 && n%2 == 0)
		value := "false"
		if busy {
			value = "true"
		}
		_, _ = w.Write([]byte(`{"total_count":1,"runners":[{"id":42,"name":"gh-runner-test-42","status":"offline","busy":` + value + `}]}`))
	}))
	defer srv.Close()
	now := time.Now()
	mgr := &fakeManager{rows: []pool.RowSnapshot{{VMID: 42, Name: "gh-runner-test-42", State: "running", StateSince: now.Add(-time.Hour)}}}
	r, err := New(baseCfg(), newTestClient(t, srv), mgr, &stubProv{}, silentLogger(), nil)
	require.NoError(t, err)
	r.now = func() time.Time { return now }
	require.NoError(t, r.Tick(context.Background()))
	now = now.Add(time.Hour)
	require.NoError(t, r.Tick(context.Background()))
	require.Empty(t, mgr.destroyCalls)
	mode.Store(0)
	require.NoError(t, r.Tick(context.Background()))
	now = now.Add(r.cfg.RunningIdleGrace + time.Second)
	calls.Store(0)
	mode.Store(2) // first list is nonbusy, fresh confirmation is busy
	require.NoError(t, r.Tick(context.Background()))
	require.Empty(t, mgr.destroyCalls)
	mode.Store(0)
	require.NoError(t, r.Tick(context.Background()))
	now = now.Add(r.cfg.RunningIdleGrace + time.Second)
	require.NoError(t, r.Tick(context.Background()))
	require.Len(t, mgr.destroyCalls, 1)
}

type stoppedTestProv struct {
	*stubProv
	state string
	err   error
}

func (s *stoppedTestProv) PowerState(context.Context, *provisioner.VM) (string, error) {
	return s.state, s.err
}

type missingTestPool struct {
	*fakeManager
	checked []pool.RowSnapshot
}

func (m *missingTestPool) ForceDestroyUnchanged(_ context.Context, row pool.RowSnapshot, _ string) error {
	m.checked = append(m.checked, row)
	return nil
}

func TestAssignedMissingRequiresCompleteGitHubAndStoppedGuest(t *testing.T) {
	for _, tc := range []struct {
		name, power, listing          string
		powerErr                      error
		protected, jitPending, reaped bool
	}{
		{name: "stopped and absent", power: "stopped", listing: "absent", reaped: true},
		{name: "powered on", power: "running", listing: "absent"},
		{name: "unconfirmed power", power: "stopped", listing: "absent", powerErr: errors.New("failed power read")},
		{name: "appears before destruction", power: "stopped", listing: "busy"},
		{name: "failed final registry check", power: "stopped", listing: "error"},
		{name: "pending JIT", power: "stopped", listing: "absent", jitPending: true},
		{name: "protected adoption", power: "stopped", listing: "absent", protected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := calls.Add(1)
				if n > 2 {
					switch tc.listing {
					case "busy":
						_, _ = w.Write([]byte(`{"total_count":1,"runners":[{"id":41,"name":"gh-runner-test-42","status":"online","busy":true}]}`))
						return
					case "error":
						w.WriteHeader(http.StatusForbidden)
						return
					}
				}
				_, _ = w.Write([]byte(`{"total_count":0,"runners":[]}`))
			}))
			defer srv.Close()
			now := time.Now()
			row := pool.RowSnapshot{VMID: 42, Node: "pve1", Name: "gh-runner-test-42", State: "assigned", StateSince: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour), JITPending: tc.jitPending, AdoptionProtected: tc.protected}
			m := &missingTestPool{fakeManager: &fakeManager{rows: []pool.RowSnapshot{row}}}
			p := &stoppedTestProv{stubProv: &stubProv{}, state: tc.power, err: tc.powerErr}
			r, err := New(baseCfg(), newTestClient(t, srv), m, p, silentLogger(), nil)
			require.NoError(t, err)
			r.now = func() time.Time { return now }
			require.NoError(t, r.Tick(context.Background()))
			require.Empty(t, m.checked, "start grace on first observed absence, not row age")
			now = now.Add(r.cfg.AssignedGrace + time.Second)
			require.NoError(t, r.Tick(context.Background()))
			if tc.reaped {
				require.Equal(t, []pool.RowSnapshot{row}, m.checked)
			} else {
				require.Empty(t, m.checked)
			}
		})
	}
}

type absentAdoptionProv struct {
	*stubProv
	absent bool
}

func (p *absentAdoptionProv) ConfirmGuestAbsent(context.Context, *provisioner.VM) (bool, error) {
	return p.absent, nil
}

type forgetAdoptionPool struct {
	*fakeManager
	forgotten []pool.RowSnapshot
}

func (p *forgetAdoptionPool) ForgetAbsentAssigned(_ context.Context, row pool.RowSnapshot) (bool, error) {
	p.forgotten = append(p.forgotten, row)
	return true, nil
}
func TestForgetAbsentAdoptionRequiresStableCompleteProof(t *testing.T) {
	now := time.Now()
	row := pool.RowSnapshot{VMID: 42, Name: "gh-runner-test-42", Node: "pve1", State: "assigned", CreatedAt: now, StateSince: now, AdoptionProtected: true}
	mgr := &forgetAdoptionPool{fakeManager: &fakeManager{rows: []pool.RowSnapshot{row}}}
	prov := &absentAdoptionProv{stubProv: &stubProv{}, absent: true}
	var present atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if present.Load() {
			_, _ = w.Write([]byte(`{"total_count":1,"runners":[{"id":42,"name":"gh-runner-test-42","status":"online","busy":true}]}`))
		} else {
			_, _ = w.Write([]byte(`{"total_count":0,"runners":[]}`))
		}
	}))
	defer srv.Close()
	r, err := New(baseCfg(), newTestClient(t, srv), mgr, prov, silentLogger(), nil)
	require.NoError(t, err)
	r.now = func() time.Time { return now }
	ctx := context.Background()
	tick := func(truncated bool) { r.applyMatrix(ctx, mgr.rows, map[string]pool.RunnerInfo{}, truncated) }
	tick(false)
	now = now.Add(r.cfg.AssignedGrace + time.Second)
	tick(true) // incomplete page invalidates the previous observation
	tick(false)
	require.Empty(t, mgr.forgotten)
	now = now.Add(r.cfg.AssignedGrace + time.Second)
	present.Store(true) // fresh confirmation vetoes stale prior listing
	tick(false)
	require.Empty(t, mgr.forgotten)
	present.Store(false)
	tick(false)
	now = now.Add(r.cfg.AssignedGrace + time.Second)
	prov.absent = false // reused VMID or unknown PVE presence
	tick(false)
	require.Empty(t, mgr.forgotten)
	prov.absent = true
	tick(false)
	require.Empty(t, mgr.forgotten)
	now = now.Add(r.cfg.AssignedGrace + time.Second)
	tick(false)
	require.Equal(t, []pool.RowSnapshot{row}, mgr.forgotten)
	require.Empty(t, mgr.destroyCalls)
}
