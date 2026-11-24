package app

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/nodecap"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/pool"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/tags"
	"github.com/stretchr/testify/require"
)

type sessionObserver struct {
	pool.Manager
	rows     []pool.RowSnapshot
	cleanups atomic.Int32
	failures atomic.Int32
}

func (m *sessionObserver) ListRows(context.Context) ([]pool.RowSnapshot, error) { return m.rows, nil }
func (m *sessionObserver) QueueObservedRestartCleanup(context.Context, pool.RowSnapshot) error {
	m.cleanups.Add(1)
	if m.failures.Add(-1) >= 0 {
		return errors.New("temporary readback unavailable")
	}
	return nil
}

const sdkSessionConflict = `failed to create message session: failed to do the session request: request POST https://example.test/_apis/runtime/runnerscalesets/2/sessions?api-version=6.0&requestId=test failed(status="409 Conflict", activity_id="test"): unexpected status code 409 Conflict: RunnerScaleSetSessionAlreadyExistsException`

func TestSharedRestartProofIgnoresLinuxSessionConflictWithoutCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var cfg config.Config
	cfg.Cluster.Mode = "standalone"
	cfg.Nodes.Strategy, cfg.Nodes.SingleNode = "single", "pve"
	cfg.Proxmox.Auth.TokenID = proofToken
	cfg.Pool.BootsPerNode = 2
	require.NoError(t, cfg.Pool.BootStartInterval.UnmarshalText([]byte("1ms")))
	require.NoError(t, cfg.Pool.CloneInflightGrace.UnmarshalText([]byte("1m")))
	cfg.Pool.RecycleMode = config.RecycleModeDestroy
	cfg.Pool.Capacity.Enabled = true
	cfg.Pool.Capacity.MemoryMode = config.MemoryModeHostAvailable
	winRange := config.VMIDRange{Min: 10001, Max: 10009}
	linRange := config.VMIDRange{Min: 11001, Max: 11009}
	cfg.Scalesets = []config.ScaleSetEntry{
		{Name: "win", VMIDRange: &winRange, Profiles: []config.ProfileConfig{{Name: "default", GuestType: "qemu"}}},
		{Name: "lin", VMIDRange: &linRange, Profiles: []config.ProfileConfig{{Name: "default", GuestType: "lxc"}}},
	}
	wire := func(set string) string {
		owner, err := tags.OwnerTag(set)
		require.NoError(t, err)
		return tags.Encode([]string{tags.Marker, owner, tags.ProfileTag("default")})
	}
	f := &recoveryFixture{active: []restartTask{}, archive: []restartTask{},
		tail:      []restartTask{receipt(proofToken, "qmstart", "9999", time.Now().Add(-3*time.Minute).Unix(), time.Now().Add(-2*time.Minute).Unix())},
		guests:    []restartGuest{{ID: 10001, Name: "gh-runner-win-10001", Tags: wire("win"), Status: "running", Uptime: 10}},
		lxcGuests: []restartGuest{{ID: 11001, Name: "gh-runner-lin-11001", Tags: wire("lin"), Status: "stopped"}},
	}
	win := &sessionObserver{rows: []pool.RowSnapshot{{VMID: 10001, Node: "pve", Name: "gh-runner-win-10001", Profile: "default", CreatedAt: time.Now(), State: "hot"}}}
	lin := &sessionObserver{rows: []pool.RowSnapshot{{VMID: 11001, Node: "pve", Name: "gh-runner-lin-11001", Profile: "default", CreatedAt: time.Now(), State: "assigned", AdoptionProtected: true, RestartCleanupPending: true}}}
	b := &restartBarrier{managers: make(map[string]pool.Manager), ready: make(chan struct{})}
	// Same production ordering: no worker/cleanup starts merely by tracking.
	defer b.track("win", win)()
	defer b.track("lin", lin)()
	boots := pool.NewHostBootBudget(2, time.Nanosecond, 0)
	boots.RequireRecovery(true)
	var retries atomic.Int32
	var linuxCanConnect atomic.Bool
	linuxDone := make(chan error, 1)
	go func() {
		_, err := openObservedMessageSession(ctx, silentLogger(), "lin", true, time.Millisecond, 5*time.Millisecond,
			func(context.Context) (*scaleset.MessageSessionClient, error) {
				retries.Add(1)
				if !linuxCanConnect.Load() {
					return nil, errors.New(sdkSessionConflict)
				}
				return &scaleset.MessageSessionClient{}, nil
			})
		if err == nil {
			b.runSessionOwnedCleanup(ctx, "lin", lin, silentLogger())
		}
		linuxDone <- err
	}()
	require.Eventually(t, func() bool { return retries.Load() >= 2 }, time.Second, time.Millisecond)
	session, err := openObservedMessageSession(ctx, silentLogger(), "win", true, time.Millisecond, 5*time.Millisecond, func(context.Context) (*scaleset.MessageSessionClient, error) {
		return &scaleset.MessageSessionClient{}, nil
	})
	require.NoError(t, err)
	require.NotNil(t, session)
	err = completeLeaderRestart(ctx, silentLogger(), &cfg, f, func(_ context.Context, v *provisioner.VM) error {
		if v.Name != "gh-runner-win-10001" {
			return errors.New("unexpected boot readiness check")
		}
		return nil
	}, 0, b, func(holds []nodecap.RestartHold, discardRemoved func()) error {
		require.Empty(t, holds)
		discardRemoved()
		boots.RequireRecovery(false)
		return nil
	})
	require.NoError(t, err)
	require.True(t, b.released())
	require.Zero(t, lin.cleanups.Load(), "shared proof does not mutate the disconnected pool")
	launch, err := boots.AcquireStart(ctx, "pve")
	require.NoError(t, err, "Windows can use shared admission while Linux still returns 409")
	launch()
	select {
	case err := <-linuxDone:
		t.Fatalf("conflict did not keep stable observer: %v", err)
	default:
	}
	linuxCanConnect.Store(true)
	select {
	case err := <-linuxDone:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Equal(t, int32(1), lin.cleanups.Load(), "deferred scoped cleanup is handed only to its session winner")
	b.runSessionOwnedCleanup(ctx, "lin", lin, silentLogger())
	require.Equal(t, int32(1), lin.cleanups.Load(), "deferred work is consumed once")
}

func TestObservedMessageSessionOnlyRetriesPinnedSDKConflict(t *testing.T) {
	require.True(t, messageSessionConflict(errors.New(sdkSessionConflict)))
	require.True(t, messageSessionConflict(errors.New(strings.Replace(sdkSessionConflict, "?api-version=6.0&requestId=test", "?[redacted]", 1))))
	require.False(t, messageSessionConflict(errors.New(strings.Replace(sdkSessionConflict, "/runnerscalesets/2/sessions", "/unrelated/2/sessions", 1))))
	require.False(t, messageSessionConflict(errors.New("error from body: "+sdkSessionConflict)))
	require.False(t, messageSessionConflict(errors.New(`failed to create message session: failed to do the session request: request POST https://example.test/sessions failed(status="403 Forbidden"): status="409 Conflict"`)))
	ctx, cancel := context.WithCancel(t.Context())
	attempts := 0
	_, err := openObservedMessageSession(ctx, silentLogger(), "lin", true, time.Hour, time.Hour, func(context.Context) (*scaleset.MessageSessionClient, error) {
		attempts++
		cancel()
		return nil, errors.New(sdkSessionConflict)
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, attempts)
}

func TestGuardedNewScaleSetCanOnlyBeCreatedAfterProof(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ready := make(chan struct{})
	first := make(chan struct{})
	done := make(chan error, 1)
	lookups, creates := 0, 0
	go func() {
		_, err := lookupGuardedScaleSet(ctx, ready, func(context.Context) (int, *scaleset.RunnerScaleSet, error) {
			lookups++
			if lookups == 1 {
				close(first)
			}
			return 7, nil, nil
		}, func(_ context.Context, group int) (*scaleset.RunnerScaleSet, error) {
			if group != 7 {
				return nil, errors.New("wrong group")
			}
			creates++
			return &scaleset.RunnerScaleSet{ID: 42}, nil
		})
		done <- err
	}()
	<-first
	select {
	case err := <-done:
		t.Fatalf("no proof expected yet: %v", err)
	default:
	}
	close(ready)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Equal(t, 2, lookups)
	require.Equal(t, 1, creates)
}

func TestSessionOwnedStoppedCleanupRetriesFreshCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	m := &sessionObserver{}
	m.failures.Store(1)
	b := &restartBarrier{managers: make(map[string]pool.Manager), ready: make(chan struct{}), stoppedCleanup: map[string][]pool.RowSnapshot{"lin": {{VMID: 11001}}}}
	defer b.track("lin", m)()
	close(b.ready)
	b.runSessionOwnedCleanup(ctx, "lin", m, silentLogger())
	require.Equal(t, int32(2), m.cleanups.Load(), "a transient readback failure cannot strand a guarded stopped row")
}
