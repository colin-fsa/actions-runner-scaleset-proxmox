package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/nodecap"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/store"
)

// This fixture drives the real managers, shared pacer and real accountant.
// Only Proxmox task/host observations and readiness are controlled. Host RAM
// includes each running guest's full shape, so retirement cannot hide pressure.
type backlogGuest struct {
	id                  int
	name, profile, upid string
	started             time.Time
}
type backlogHost struct {
	pressure                            uint64
	mu                                  sync.Mutex
	guests                              map[int]*backlogGuest
	pending                             map[int]bool
	clonePosts                          []time.Time
	activeClones, maxClones, maxPending int
	maxMemory                           uint64
	firstClones, slowClones, readiness  chan struct{}
	ready                               chan int
	posted                              []int
}

func (h *backlogHost) memoryLocked() uint64 {
	var used uint64
	for _, g := range h.guests {
		if !g.started.IsZero() {
			used += backlogShape(g.profile)
		}
	}
	return used
}
func backlogShape(profile string) uint64 {
	if profile == "windows" {
		return 4 * gib
	}
	return 10 * gib
}
func (h *backlogHost) Write(p []byte) (int, error) {
	var event struct {
		Msg  string `json:"msg"`
		VMID int    `json:"vmid"`
	}
	if json.Unmarshal(p, &event) == nil && event.Msg == "nodecap: retired launch hold after direct host visibility" {
		h.mu.Lock()
		delete(h.pending, event.VMID)
		h.mu.Unlock()
	}
	return len(p), nil
}
func (h *backlogHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	switch {
	case r.URL.Path == "/cluster/resources":
		rows := []map[string]any{{"type": "node", "node": "pve1", "status": "online", "maxmem": 128 * gib, "maxcpu": 32}}
		for _, g := range h.guests {
			status := "stopped"
			if !g.started.IsZero() {
				status = "running"
			}
			rows = append(rows, map[string]any{"type": "qemu", "node": "pve1", "vmid": g.id, "name": g.name, "status": status, "maxmem": backlogShape(g.profile), "maxcpu": 4})
		}
		write(rows)
	case r.URL.Path == "/nodes/pve1/status":
		write(map[string]any{"memory": map[string]any{"total": 128 * gib, "available": 128*gib - h.memoryLocked() - h.pressure}, "cpu": 0.1})
	case strings.Contains(r.URL.Path, "/tasks/"):
		upid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/nodes/pve1/tasks/"), "/status")
		for _, g := range h.guests {
			if g.upid == upid {
				write(map[string]any{"upid": upid, "node": "pve1", "type": "qmstart", "id": strconv.Itoa(g.id), "starttime": g.started.Unix(), "status": "stopped", "exitstatus": "OK"})
				return
			}
		}
		http.Error(w, "unknown exact receipt", 404)
	case strings.HasSuffix(r.URL.Path, "/status/current"):
		parts := strings.Split(r.URL.Path, "/")
		id, _ := strconv.Atoi(parts[4])
		g := h.guests[id]
		if g == nil {
			http.Error(w, "unknown guest", 404)
			return
		}
		status := "stopped"
		pid, uptime := 0, int64(0)
		if !g.started.IsZero() {
			status = "running"
			pid = id + 1000
			uptime = max(1, int64(time.Since(g.started)/time.Second))
		}
		write(map[string]any{"vmid": id, "name": g.name, "status": status, "pid": pid, "uptime": uptime, "ha": map[string]int{"managed": 0}})
	default:
		http.Error(w, "unexpected read", 400)
	}
}

type backlogProv struct {
	*fakeProv
	h *backlogHost
}

func (p *backlogProv) Clone(ctx context.Context, opts provisioner.CloneOptions) (*provisioner.VM, error) {
	var batch int
	err := opts.Submission(ctx, func(context.Context) error {
		p.h.mu.Lock()
		defer p.h.mu.Unlock()
		batch = len(p.h.clonePosts) / 2
		p.h.clonePosts = append(p.h.clonePosts, time.Now())
		p.h.activeClones++
		p.h.maxClones = max(p.h.maxClones, p.h.activeClones)
		return nil
	})
	if err != nil {
		return nil, err
	}
	defer func() { p.h.mu.Lock(); p.h.activeClones--; p.h.mu.Unlock() }()
	release := p.h.slowClones
	if batch == 0 {
		release = p.h.firstClones
	}
	select {
	case <-release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.h.mu.Lock()
	p.h.guests[opts.NewVMID] = &backlogGuest{id: opts.NewVMID, name: opts.Name, profile: opts.Profile}
	p.h.mu.Unlock()
	return &provisioner.VM{VMID: opts.NewVMID, Node: opts.Node, Name: opts.Name}, nil
}
func (p *backlogProv) StartWithSubmission(ctx context.Context, vm *provisioner.VM, gate provisioner.StartSubmission) error {
	return gate(ctx, func(ctx context.Context) error {
		h := p.h
		h.mu.Lock()
		g := h.guests[vm.VMID]
		g.started = time.Now()
		g.upid = fmt.Sprintf("UPID:pve1:00000001:00000002:%08X:qmstart:%d:runner@pve!test:", g.started.Unix(), g.id)
		upid := g.upid
		h.pending[g.id] = true
		h.maxPending = max(h.maxPending, len(h.pending))
		h.maxMemory = max(h.maxMemory, h.memoryLocked())
		h.posted = append(h.posted, g.id)
		h.mu.Unlock()
		nodecap.ReportPacedTask(ctx, "pve1", vm.VMID, upid, false)
		nodecap.ReportPacedTask(ctx, "pve1", vm.VMID, upid, true)
		return nil
	})
}
func (p *backlogProv) WaitReady(ctx context.Context, vm *provisioner.VM, _ time.Duration) error {
	p.h.ready <- vm.VMID
	select {
	case <-p.h.readiness:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestMixedBacklogBoundsPressureAndMakesColdProgress(t *testing.T) {
	const interval = 20 * time.Millisecond
	h := &backlogHost{guests: make(map[int]*backlogGuest), pending: make(map[int]bool), firstClones: make(chan struct{}), slowClones: make(chan struct{}), readiness: make(chan struct{}), ready: make(chan int, 100)}
	releaseFirst := sync.OnceFunc(func() { close(h.firstClones) })
	releaseSlow := sync.OnceFunc(func() { close(h.slowClones) })
	releaseReady := sync.OnceFunc(func() { close(h.readiness) })
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	acct, err := nodecap.New(proxmox.NewClient(srv.URL), nodecap.Options{MemoryMode: nodecap.MemoryModeHostAvailable, ReserveBytes: 4 * gib, Refresh: time.Second, CPUMode: nodecap.CPUModeHostUsage, CPUUsageLimit: 0.8, CPULaunchVCPUs: 1, PacedStartLimit: 2, PacedStartInterval: interval, PacedTaskOwner: "runner@pve!test", Log: slog.New(slog.NewJSONHandler(h, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	require.NoError(t, err)
	budget := NewHostBootBudget(2, interval, 0)
	var managers []*manager
	for i, profile := range []string{"linux", "windows"} {
		st := newTestStore(t)
		p := &backlogProv{fakeProv: &fakeProv{}, h: h}
		m := newTestManager(t, st, p, Config{ScaleSetName: profile, LinkedClones: true, MaxConcurrentRunners: 128, VMIDRange: config.VMIDRange{Min: 10000 + i*1000, Max: 10999 + i*1000}, Capacity: acct, WindowsBootBudget: budget, Profiles: []ProfileSettings{{Name: profile, GuestOS: profile, MemoryMB: int(backlogShape(profile) / (1024 * 1024)), CPUCores: 4, MaxConcurrentRunners: 128}}})
		managers = append(managers, m)
	}
	// Cancel and join every manager worker even if an assertion fails.
	t.Cleanup(func() {
		releaseFirst()
		releaseSlow()
		releaseReady()
		for _, m := range managers {
			m.wg.Wait()
			m.Close()
		}
	})
	readCounts := func() (int, int) { h.mu.Lock(); defer h.mu.Unlock(); return len(h.clonePosts), h.activeClones }
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	for _, m := range managers {
		m.kickClone(ctx, m.defaultProfile(), store.PoolKindHot, true)
	}
	require.Eventually(t, func() bool { posts, active := readCounts(); return posts == 2 && active == 2 }, time.Second, time.Millisecond)
	// Queue another pair while the first two clone tasks overlap. They must
	// wait on a separate host clone bound, not in front of warm submissions.
	for _, m := range managers {
		m.kickClone(ctx, m.defaultProfile(), store.PoolKindHot, true)
	}
	time.Sleep(3 * interval)
	posts, active := readCounts()
	require.Equal(t, 2, posts)
	require.Equal(t, 2, active)
	releaseFirst()
	// First two cold guests are powered on and stay in readiness throughout.
	for range 2 {
		select {
		case <-h.ready:
		case <-ctx.Done():
			t.Fatal("initial cold start did not progress")
		}
	}
	require.Eventually(t, func() bool { posts, active := readCounts(); return posts == 4 && active == 2 }, time.Second, time.Millisecond)
	// Sustained warm arrivals and repeated real reconciliation attempts. The
	// accountant's 5s exact-probe throttle is intentionally not shortened.
	// New warm starts must occur after fresh accounting settles the cold
	// pair, despite both cold readiness workers and both slow clone tasks.
	firstWarm := false
	coldProgress := false
	arrivals := 0
	deadline := time.Now().Add(25 * time.Second)
	nextArrival := time.Now()
	slowReleased := false
	for time.Now().Before(deadline) && !coldProgress {
		if !time.Now().Before(nextArrival) {
			arrivals++
			for i, m := range managers {
				id := 10500 + i*1000 + arrivals
				name := fmt.Sprintf("warm-%d", id)
				now := time.Now()
				require.NoError(t, m.store.Insert(&store.VM{VMID: id, Node: "pve1", Name: name, Profile: m.defaultProfile(), State: store.StateWarm, PoolKind: store.PoolKindWarm, CreatedAt: now, StateSince: now}))
				h.mu.Lock()
				h.guests[id] = &backlogGuest{id: id, name: name, profile: m.defaultProfile()}
				h.mu.Unlock()
			}
			nextArrival = time.Now().Add(300 * time.Millisecond)
		}
		for _, m := range managers {
			m.promoteN(ctx, m.defaultProfile(), 1)
		}
		h.mu.Lock()
		warmDuringClones := false
		warmCount, extraCold := 0, 0
		for _, id := range h.posted {
			if id >= 10500 && id < 11000 || id >= 11500 {
				warmCount++
			} else if id == 10001 || id == 11001 {
				extraCold++
			}
		}
		if warmCount > 0 && !firstWarm {
			firstWarm = true
			warmDuringClones = h.activeClones == 2 && len(h.posted) >= 3
		}
		coldProgress = extraCold == 2
		h.mu.Unlock()
		if firstWarm && !slowReleased {
			require.True(t, warmDuringClones, "warm must progress while both slow clone tasks and cold readiness overlap")
		}
		if firstWarm && !slowReleased {
			releaseSlow()
			slowReleased = true
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.True(t, firstWarm, "warm backlog never passed reliable accounting while cold guests stayed in readiness")
	require.True(t, coldProgress, "both queued cold clones must start during sustained warm arrivals")
	require.Greater(t, arrivals, 2, "warm demand must continue beyond one initial burst")
	// Tighten actual contemporaneous host availability to the reserve, after
	// the completed cold work has demonstrably progressed. Serialize this
	// fixture change with the real POST boundary, not an in-flight old GET.
	n := budget.node("pve1")
	n.pace.Lock()
	h.mu.Lock()
	h.pressure = 124*gib - h.memoryLocked()
	beforePressure := len(h.posted)
	h.mu.Unlock()
	n.pace.Unlock()
	time.Sleep(1100 * time.Millisecond) // expire the ordinary snapshot cache
	states, err := acct.Snapshot(ctx)
	require.NoError(t, err)
	require.Zero(t, states["pve1"].AvailableBytes, "fresh actual host pressure must close admission")
	for range 4 {
		for _, m := range managers {
			m.promoteN(ctx, m.defaultProfile(), 1)
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.mu.Lock()
	afterPressure := len(h.posted)
	h.mu.Unlock()
	require.Equal(t, beforePressure, afterPressure, "ongoing warm demand cannot start below the actual host reserve")

	h.mu.Lock()
	maxClones, maxPending, maxMemory, powered := h.maxClones, h.maxPending, h.maxMemory, len(h.posted)
	clonePosts := append([]time.Time(nil), h.clonePosts...)
	h.mu.Unlock()
	require.Equal(t, 2, maxClones, "clone tasks are bounded across both scale sets")
	require.Equal(t, 2, maxPending, "atomic outstanding-start bound remains two after accounting retirement")
	require.LessOrEqual(t, maxMemory, uint64(124*gib), "all actually running shapes preserve the 4 GiB host reserve")
	for i := 1; i < len(clonePosts); i++ {
		require.GreaterOrEqual(t, clonePosts[i].Sub(clonePosts[i-1]), interval)
	}
	t.Logf("mixed backlog: arrivals=%d, powered=%d, max clone tasks=%d, max outstanding starts=%d, max actual guest RAM=%.0f GiB; every readiness worker still blocked", arrivals*2, powered, maxClones, maxPending, float64(maxMemory)/float64(gib))
	releaseReady()
}
