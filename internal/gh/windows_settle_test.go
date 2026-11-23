package gh

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/pool"
)

type windowsSettleManager struct {
	*fakeManager
	observed []pool.RunnerInfo
	polls    []time.Time
}

func (*windowsSettleManager) WindowsOnlineSettleEnabled() bool { return true }
func (m *windowsSettleManager) ObserveWindowsRunner(_ pool.RowSnapshot, gr pool.RunnerInfo, started time.Time) {
	m.observed = append(m.observed, gr)
	m.polls = append(m.polls, started)
}

func TestWindowsSettleTickRevokesOnGitHubListFailure(t *testing.T) {
	t.Parallel()
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if fail.Load() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"forbidden"}`)
			return
		}
		_, _ = io.WriteString(w, `{"total_count":1,"runners":[{"id":100,"name":"gh-runner-test-1001","status":"online","busy":true}]}`)
	}))
	t.Cleanup(srv.Close)

	m := &windowsSettleManager{fakeManager: &fakeManager{rows: []pool.RowSnapshot{
		{VMID: 1001, Name: "gh-runner-test-1001", State: "running", RunnerID: 100},
	}}}
	r, err := New(baseCfg(), newTestClient(t, srv), m, &stubProv{}, silentLogger(), nil)
	require.NoError(t, err)
	current := time.Now()
	r.now = func() time.Time { return current }
	require.NoError(t, r.Tick(context.Background()))
	require.Equal(t, []pool.RunnerInfo{{ID: 100, Online: true, Busy: true}}, m.observed)
	require.Equal(t, []time.Time{current}, m.polls)
	fail.Store(true)
	current = current.Add(time.Second)
	require.Error(t, r.Tick(context.Background()))
	require.Equal(t, pool.RunnerInfo{}, m.observed[1])
	require.Equal(t, current, m.polls[1])
}
