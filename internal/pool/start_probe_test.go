package pool

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/nodecap"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/store"
)

// Use the actual HTTP accountant and pool start workers, not a fake retry
// counter. Only PVE latency/pressure/error responses are controlled.
func probeManager(t *testing.T, h *backlogHost, handler http.Handler, b *WindowsBootBudget) *manager {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	acct, err := nodecap.New(proxmox.NewClient(srv.URL), nodecap.Options{MemoryMode: nodecap.MemoryModeHostAvailable, ReserveBytes: 4 * gib, Refresh: time.Second, PacedStartLimit: 2, PacedStartInterval: time.Millisecond, PacedTaskOwner: "runner@pve!test", Log: slog.New(slog.NewJSONHandler(h, nil))})
	require.NoError(t, err)
	m := newTestManager(t, newTestStore(t), &backlogProv{fakeProv: &fakeProv{}, h: h}, Config{Capacity: acct, WindowsBootBudget: b, MaxConcurrentRunners: 128, Profiles: []ProfileSettings{{Name: "linux", MemoryMB: 10240, MaxConcurrentRunners: 128}, {Name: "windows", MemoryMB: 4096, MaxConcurrentRunners: 128}}})
	t.Cleanup(m.Close)
	return m
}
func probeHost() *backlogHost {
	return &backlogHost{guests: make(map[int]*backlogGuest), pending: make(map[int]bool)}
}
func runProbeWorker(ctx context.Context, m *manager, h *backlogHost, id int, profile string) error {
	deadline, _ := ctx.Deadline()
	now := time.Now()
	row := &store.VM{VMID: id, Node: "pve1", Name: profile + "-probe", Profile: profile, State: store.StateBooting, PoolKind: store.PoolKindHot, CreatedAt: now, StartWorkerDeadline: deadline}
	if err := m.store.Insert(row); err != nil {
		return err
	}
	h.mu.Lock()
	h.guests[id] = &backlogGuest{id: id, name: row.Name, profile: profile}
	h.mu.Unlock()
	_, _, err := m.startBootWorker(ctx, &provisioner.VM{VMID: id, Node: row.Node, Name: row.Name}, row, m.profileOf(profile), profile == "windows", func() bool { return true })
	return err
}
func waitProbeWorkers(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cold cancellation left workers queued")
	}
}

func TestColdPressureCoalescesProxmoxReadsAndWarmStillStarts(t *testing.T) {
	h := probeHost()
	h.pressure = 124 * gib // at the actual host floor: all starts must wait.
	var reads, total atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		total.Add(1)
		if r.URL.Path == "/cluster/resources" {
			reads.Add(1)
		}
		h.ServeHTTP(w, r)
	})
	b := NewHostBootBudget(2, time.Millisecond, 0)
	m := probeManager(t, h, handler, b)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Go(func() {
			err := runProbeWorker(ctx, m, h, 10000+i, "linux")
			if !errors.Is(err, errStartCapacityDeferred) {
				t.Errorf("cold refusal/cancel: %v", err)
			}
		})
	}
	require.Eventually(t, func() bool {
		n := b.node("pve1")
		n.probeMu.Lock()
		defer n.probeMu.Unlock()
		return n.coldProbe.retry != nil
	}, time.Second, time.Millisecond)
	time.Sleep(2200 * time.Millisecond)
	cancel()
	waitProbeWorkers(t, &wg)
	h.mu.Lock()
	h.pressure = 120 * gib
	h.mu.Unlock()
	warmCtx, warmCancel := context.WithTimeout(t.Context(), time.Second)
	defer warmCancel()
	require.NoError(t, runProbeWorker(warmCtx, m, h, 15000, "windows"), "an independent warm probe sees recovered actual headroom")
	require.LessOrEqual(t, reads.Load(), int32(6), "node/cohort bounds, not 10 direct reads/second per cold worker")
	require.LessOrEqual(t, total.Load(), int32(20), "includes host and exact task/process rechecks")
	h.mu.Lock()
	posted := append([]int(nil), h.posted...)
	h.mu.Unlock()
	require.Equal(t, []int{15000}, posted)
	t.Logf("24 pressured cold workers: cluster reads=%d total PVE GETs=%d; warm POST progressed", reads.Load(), total.Load())
}

func TestColdReadErrorsBackOffPerNodeAndCancellationIsPrompt(t *testing.T) {
	h := probeHost()
	var reads atomic.Int32
	b := NewHostBootBudget(2, time.Millisecond, 0)
	m := probeManager(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reads.Add(1); http.Error(w, "unavailable", 400) }), b)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Go(func() {
			err := runProbeWorker(ctx, m, h, 10000+i, "linux")
			if !errors.Is(err, errStartCapacityDeferred) {
				t.Errorf("cold read/cancel: %v", err)
			}
		})
	}
	time.Sleep(2200 * time.Millisecond)
	n := b.node("pve1")
	n.probeMu.Lock()
	retry := n.coldProbe.retry
	failures := n.coldProbe.readFailures
	n.probeMu.Unlock()
	require.NotNil(t, retry)
	require.ErrorIs(t, retry, nodecap.ErrPacedStartRead)
	require.GreaterOrEqual(t, failures, 2)
	require.LessOrEqual(t, reads.Load(), int32(3), "read-error probes use 1s, 2s, 4s, 5s bounded node backoff")
	started := time.Now()
	cancel()
	waitProbeWorkers(t, &wg)
	require.Less(t, time.Since(started), time.Second)
	n.pace.Lock()
	require.Empty(t, n.coldWaiting)
	n.pace.Unlock()
	t.Logf("24 failed cold workers: PVE GETs=%d, failure level=%d", reads.Load(), failures)
}

func TestSlowColdReadDoesNotOwnWarmPostLockOrWarmReadLane(t *testing.T) {
	h := probeHost()
	h.pressure = 120 * gib
	b := NewHostBootBudget(2, time.Millisecond, 0)
	entered, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cluster/resources" && reads.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		h.ServeHTTP(w, r)
	})
	m := probeManager(t, h, handler, b)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	coldDone := make(chan error, 1)
	go func() { coldDone <- runProbeWorker(ctx, m, h, 10000, "linux") }()
	<-entered
	warmDone := make(chan error, 1)
	go func() { warmDone <- runProbeWorker(ctx, m, h, 15000, "windows") }()
	select {
	case err := <-warmDone:
		require.NoError(t, err)
	case <-time.After(500 * time.Millisecond):
		close(release)
		t.Fatal("warm queued behind a slow cold GET at the shared POST lock")
	}
	close(release)
	cancel()
	require.ErrorIs(t, <-coldDone, errStartCapacityDeferred)
	require.Equal(t, int32(2), reads.Load(), "one coalesced cold read and independent fresh warm read")
}

func TestUnsettledOwnershipClaimsDoNotBlockMeasuredHeadroom(t *testing.T) {
	h := probeHost()
	var reads atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cluster/resources" {
			reads.Add(1)
		}
		h.ServeHTTP(w, r)
	})
	b := NewHostBootBudget(2, time.Millisecond, 0)
	m := probeManager(t, h, handler, b)
	acct := m.cfg.Capacity.(*nodecap.Accountant)
	sample, err := acct.SamplePacedStart(t.Context(), "pve1")
	require.NoError(t, err)
	for i := range 2 {
		r, ok, err := sample.Reserve(t.Context(), "pve1", nodecap.Shape{MemoryBytes: 10 * gib}, 20000+i, time.Now(), false)
		require.NoError(t, err)
		require.True(t, ok)
		require.True(t, claimFor(r).beginStart())
	}
	initialReads := reads.Load()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			err := runProbeWorker(ctx, m, h, 10000+i, "linux")
			if err != nil && !errors.Is(err, errStartCapacityDeferred) {
				t.Errorf("slot wait: %v", err)
			}
		})
	}
	time.Sleep(2200 * time.Millisecond)
	n := b.node("pve1")
	n.probeMu.Lock()
	retry := n.coldProbe.retry
	failures := n.coldProbe.readFailures
	n.probeMu.Unlock()
	require.Nil(t, retry, "unresolved ownership claims are not capacity pressure")
	require.Zero(t, failures, "slot saturation is not an API outage")
	require.LessOrEqual(t, reads.Load()-initialReads, int32(8), "workers share bounded fresh probes")
	// Exercise exact cleanup to remove the simulated outstanding starts.
	for i := range 2 {
		acct.ReservationsBeforeDestroy("pve1", 20000+i)()
	}
	require.Eventually(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.posted) == 4 }, 1500*time.Millisecond, 10*time.Millisecond, "cold work must not wait for unrelated ownership cleanup")
	cancel()
	waitProbeWorkers(t, &wg)
}

func TestCancellingProbeOwnerDoesNotDropOtherColdWorkers(t *testing.T) {
	h := probeHost()
	b := NewHostBootBudget(2, time.Millisecond, 0)
	entered, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cluster/resources" {
			reads.Add(1)
			select {
			case <-entered:
			default:
				close(entered)
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		h.ServeHTTP(w, r)
	})
	m := probeManager(t, h, handler, b)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ownerCtx, cancelOwner := context.WithCancel(ctx)
	owner, peer := make(chan error, 1), make(chan error, 1)
	go func() { owner <- runProbeWorker(ownerCtx, m, h, 10000, "linux") }()
	<-entered
	go func() { peer <- runProbeWorker(ctx, m, h, 10001, "linux") }()
	require.Eventually(t, func() bool { n := b.node("pve1"); n.pace.Lock(); defer n.pace.Unlock(); return len(n.coldWaiting) == 2 }, time.Second, time.Millisecond)
	cancelOwner()
	select {
	case err := <-owner:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(250 * time.Millisecond):
		close(release)
		t.Fatal("cancelled owner waited for shared Proxmox I/O")
	}
	close(release)
	require.NoError(t, <-peer, "owner cancellation must not reclassify peers as Warm")
	require.LessOrEqual(t, reads.Load(), int32(2), "a peer that arrives after completion may take its own fresh sample")
	h.mu.Lock()
	posted := append([]int(nil), h.posted...)
	h.mu.Unlock()
	require.Equal(t, []int{10001}, posted)
}

func TestCompletingOldReadCannotEraseNewerSlotCooldown(t *testing.T) {
	b := NewHostBootBudget(2, time.Millisecond, 0)
	entered, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	read := func(context.Context, string) (nodecap.PacedStartSample, error) {
		if reads.Add(1) == 1 {
			close(entered)
		}
		<-release
		return stubStartSample{a: &pacedAdmitterStub{}}, nil
	}
	done := make(chan error, 1)
	go func() { _, err := b.sampleStart(t.Context(), "pve1", false, read); done <- err }()
	<-entered
	deferred := b.deferStart("pve1", false, nodecap.ErrPacedStartSlots)
	close(release)
	require.NoError(t, <-done)
	_, err := b.sampleStart(t.Context(), "pve1", false, read)
	require.Same(t, deferred, err, "a pre-existing flight must not reset a later node cooldown")
	require.Equal(t, int32(1), reads.Load())
}

func TestPreparedWorkerChecksGenerationAgainAfterSlowRead(t *testing.T) {
	h := probeHost()
	b := NewHostBootBudget(2, time.Millisecond, 0)
	entered, release := make(chan struct{}), make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cluster/resources" {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		h.ServeHTTP(w, r)
	})
	m := probeManager(t, h, handler, b)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runProbeWorker(ctx, m, h, 15000, "windows") }()
	<-entered
	replacement, err := m.store.Get(15000)
	require.NoError(t, err)
	require.NoError(t, m.store.Delete(15000))
	replacement.CreatedAt = replacement.CreatedAt.Add(time.Second)
	require.NoError(t, m.store.Insert(replacement))
	close(release)
	require.ErrorIs(t, <-done, errStartCapacityDeferred)
	h.mu.Lock()
	defer h.mu.Unlock()
	require.Empty(t, h.posted, "a queued sample must not start a reused VMID")
}
