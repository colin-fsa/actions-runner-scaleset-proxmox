package nodecap

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWindowsSettledCreditRequiresFreshHostAndOnlineEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rows := []map[string]any{nodeRow("pve1", 64*gib, 20)}
	for i := 1; i <= 6; i++ {
		g := guestRow(12000+i, "pve1", 4*gib, 4)
		g["name"] = fmt.Sprintf("win-%d", i)
		g["uptime"] = 30
		rows = append(rows, g)
	}
	rs := newResourceServer(t, rows)
	rs.setNodeStatus("pve1", 64*gib, 64*gib)
	rs.setNodeCPU("pve1", .25)
	a := newAccountant(t, rs, func(o *Options) {
		o.MemoryMode = MemoryModeHostAvailable
		o.CPUMode = CPUModeHostUsage
		o.CPUUsageLimit = .8
		o.ReserveBytes = 8 * gib
		o.ReservationTTL = 15 * time.Minute
		o.Refresh = 15 * time.Second
		o.WindowsOnlineSettleAfter = 30 * time.Second
		o.WindowsOnlineFreshness = time.Minute
	})
	now := time.Now()
	a.now = func() time.Time { return now }
	gen := now.Add(-time.Second)
	win := Shape{MemoryBytes: 4 * gib}
	for i := 1; i <= 6; i++ {
		r, ok, err := a.Reserve(ctx, "pve1", win)
		require.NoError(t, err)
		require.True(t, ok)
		r.Bind(12000 + i)
		x := r.(*reservation)
		x.SetGeneration(gen)
		require.True(t, x.BeginStart())
	}
	now = now.Add(5 * time.Second)
	delivered := now
	now = now.Add(time.Second)
	first := now
	observe := func() {
		t.Helper()
		for i := 1; i <= 6; i++ {
			a.ObserveWindowsOnline("pve1", fmt.Sprintf("win-%d", i), 12000+i, gen, delivered, now)
		}
	}
	observe()
	rs.setNodeStatus("pve1", 30*gib, 64*gib) // 22 GiB gross, 24 GiB full holds
	rs.setNodeCPU("pve1", .25)
	available := func(fresh bool) uint64 {
		t.Helper()
		if fresh {
			a.cache.Delete(snapshotCacheKey)
		}
		s, err := a.Snapshot(ctx)
		require.NoError(t, err)
		return s["pve1"].AvailableBytes
	}
	require.Zero(t, available(true), "first registry poll cannot give credit")
	now = first.Add(29 * time.Second)
	observe()
	require.Zero(t, available(true), "settle interval not yet reached")
	now = first.Add(30 * time.Second)
	require.Zero(t, available(false), "pre-deadline PVE sample never proves host accounting")
	require.Equal(t, uint64(14*gib), available(true), "16 GiB cap admits one Linux clone, never all 24 GiB")
	choices, err := a.Fits(ctx, Shape{MemoryBytes: 10 * gib}, []string{"pve1"})
	require.NoError(t, err)
	require.True(t, choices["pve1"])
	require.Len(t, a.res, 6, "holds are not actually released early")

	rs.setNodeCPU("pve1", .9)
	require.Zero(t, available(true), "high CPU suppresses the relaxed credit")
	rs.setNodeCPU("pve1", .25)
	for i := 1; i <= 3; i++ {
		a.ForgetWindowsOnline("pve1", 12000+i, gen, now)
	}
	require.Equal(t, uint64(10*gib), available(true), "three proven current Windows generations remain")
	a.ForgetWindowsOnline("pve1", 12004, gen.Add(time.Second), now)
	require.Equal(t, uint64(10*gib), available(false), "unrelated generation cannot revoke proof")
	a.ForgetWindowsOnline("pve1", 12004, gen, now)
	require.Equal(t, uint64(6*gib), available(false), "unknown/offline evidence restores its hold")
	rows[5]["name"] = "reused-vmid"
	rs.setRows(rows)
	require.Equal(t, uint64(2*gib), available(true), "fresh PVE guest identity must match")
	rows[6]["uptime"] = 1
	rs.setRows(rows)
	require.Zero(t, available(true), "same name with a restarted QEMU process cannot retain the credit")
	now = now.Add(2 * time.Minute)
	require.Zero(t, available(true), "stale registry observations cannot earn credit")
	a.BeginRecovery(time.Minute)
	choices, err = a.Fits(ctx, Shape{MemoryBytes: gib}, []string{"pve1"})
	require.NoError(t, err)
	require.False(t, choices["pve1"])
}

func TestWindowsSettledCreditIgnoresUnstartedAndNewerGeneration(t *testing.T) {
	t.Parallel()
	now := time.Now()
	gen := now.Add(-time.Minute)
	a := &Accountant{
		opts: Options{MemoryMode: MemoryModeHostAvailable, WindowsOnlineSettleAfter: time.Minute},
		now:  func() time.Time { return now },
		res: map[uint64]*reservation{
			1: {id: 1, node: "pve1", vmid: 12001, generation: gen, expires: now.Add(time.Minute)},
			2: {id: 2, node: "pve1", vmid: 12001, generation: gen.Add(time.Second), started: true, startedAt: gen, expires: now.Add(time.Minute)},
		},
	}
	a.ObserveWindowsOnline("pve1", "test", 12001, gen, now.Add(-time.Second), now)
	require.True(t, a.res[1].online.first.IsZero())
	require.True(t, a.res[2].online.first.IsZero())
}
