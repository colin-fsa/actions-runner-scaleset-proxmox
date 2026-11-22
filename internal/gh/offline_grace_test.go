package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/pool"
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
