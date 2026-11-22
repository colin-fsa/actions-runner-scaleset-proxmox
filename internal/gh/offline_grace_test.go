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
		_, _ = w.Write([]byte(`{"total_count":1,"runners":[{"id":111,"name":"gh-runner-test-2001","status":"` + status.Load().(string) + `","busy":true}]}`))
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
