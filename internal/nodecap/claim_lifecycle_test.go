package nodecap

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPromotionTransfersExactWarmClaimBeforeInventoryCatchesUp(t *testing.T) {
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
	rs.setNodeStatus("pve1", 14*gib, 32*gib)
	a := newAccountant(t, rs, func(o *Options) {
		o.MemoryMode, o.ReserveBytes, o.Refresh = MemoryModeHostAvailable, 4*gib, time.Minute
	})
	ctx := context.Background()
	shape := Shape{MemoryBytes: 10 * gib, VCPUs: 4}
	generation := time.Now()
	h, ok, err := a.Reserve(ctx, "pve1", shape)
	require.NoError(t, err)
	require.True(t, ok)
	r := h.(*reservation)
	r.Bind(10001)
	r.SetGeneration(generation)
	r.ExpectPoweredOff()
	_, ok, err = a.ReserveExistingForPromotion(ctx, "pve1", shape, 10001, generation.Add(-time.Second))
	require.NoError(t, err)
	require.False(t, ok, "an old generation cannot use the clone's paid claim")
	transfer, ok, err := a.ReserveExistingForPromotion(ctx, "pve1", shape, 10001, generation)
	require.NoError(t, err)
	require.True(t, ok, "the 10 GiB was already paid by this exact stopped clone")
	require.Same(t, r, transfer)
	require.Len(t, a.res, 1)
	_, ok, err = a.ReserveExistingForPromotion(ctx, "pve1", shape, 10001, generation)
	require.NoError(t, err)
	require.False(t, ok, "another caller cannot borrow the same pending promotion")
	rs.setRows([]map[string]any{nodeRow("pve1", 32*gib, 16), stoppedGuestRow(10001, "pve1", 10*gib, 4)})
	a.cache.Delete(snapshotCacheKey)
	st, err := a.Snapshot(ctx)
	require.NoError(t, err)
	require.Zero(t, st["pve1"].AvailableBytes, "an intervening stopped snapshot must not sweep a promoted claim")
	require.True(t, r.BeginStart())
	// An existing start is borrowed, not transferred or double-counted.
	borrowed, ok, err := a.ReserveExistingForJIT(ctx, "pve1", shape, 10001, generation)
	require.NoError(t, err)
	require.True(t, ok)
	borrowed.Release()
	require.Len(t, a.res, 1)
	a.ReservationsBeforeDestroy("pve1", 10001)()
	require.Empty(t, a.res)
}

func TestFortyFourGiBStartedClaimsRemainUntilExactLifecycleEvidence(t *testing.T) {
	const hostReserve = 25_284_563_558
	const observedHostAvail = 72_745_156_608 // 67.75 GiB, rounded to a KiB counter
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 157*gib, 88)})
	rs.setNodeStatus("pve1", observedHostAvail, 157*gib)
	var log bytes.Buffer
	a := newAccountant(t, rs, func(o *Options) {
		o.MemoryMode, o.ReserveBytes, o.Refresh = MemoryModeHostAvailable, hostReserve, time.Minute
		o.Log = slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	ctx := context.Background()
	linux := Shape{MemoryBytes: 10 * gib, VCPUs: 4}
	windows := Shape{MemoryBytes: 4 * gib, VCPUs: 4}
	generation := time.Now()
	rows := []map[string]any{nodeRow("pve1", 157*gib, 88)}
	for i, shape := range []Shape{linux, windows, linux, linux, linux} {
		vmid := 10001 + i
		h, ok, err := a.Reserve(ctx, "pve1", shape)
		require.NoError(t, err)
		require.True(t, ok)
		r := h.(*reservation)
		r.Bind(vmid)
		r.SetGeneration(generation)
		require.True(t, r.BeginStart())
		row := guestRow(vmid, "pve1", shape.MemoryBytes, 4)
		row["mem"] = shape.MemoryBytes / 3 // guest-reported cgroup usage is not exact resident attribution
		rows = append(rows, row)
	}
	rs.setRows(rows)
	a.cache.Delete(snapshotCacheKey)
	_, ok, err := a.Reserve(ctx, "pve1", linux)
	require.NoError(t, err)
	require.False(t, ok, "a fresh node-wide sample and positive power state are not evidence to net off future growth")
	st, err := a.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(observedHostAvail-hostReserve-44*gib), st["pve1"].AvailableBytes)
	var evidence struct {
		Pending uint64                   `json:"pending_claim_shape_bytes"`
		Started uint64                   `json:"started_claim_shape_bytes"`
		Claims  []admissionClaimEvidence `json:"claims"`
	}
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(log.Bytes()), &evidence))
	require.Zero(t, evidence.Pending)
	require.Equal(t, uint64(44*gib), evidence.Started)
	require.Len(t, evidence.Claims, 5)
	require.Equal(t, 10001, evidence.Claims[0].VMID)
	require.Equal(t, "start_issued_or_issuing", evidence.Claims[0].Phase)
	require.True(t, evidence.Claims[0].InventoryNonstopped)

	confirmed := a.ReservationsBeforeDestroy("pve1", 10001)
	_, ok, err = a.Reserve(ctx, "pve1", linux)
	require.NoError(t, err)
	require.False(t, ok, "a failed or not-yet-confirmed teardown retains its hold")
	confirmed()
	_, ok, err = a.Reserve(ctx, "pve1", linux)
	require.NoError(t, err)
	require.True(t, ok, "confirmed removal returns exactly one 10 GiB slot")
	require.Len(t, a.res, 5)
}

func TestPacedStartRejectsCachedCapacityWhenFreshSourceFails(t *testing.T) {
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
	rs.setNodeStatus("pve1", 24*gib, 32*gib)
	a := newAccountant(t, rs, func(o *Options) {
		o.MemoryMode, o.ReserveBytes, o.Refresh = MemoryModeHostAvailable, 4*gib, time.Minute
		o.PacedStartLimit, o.PacedStartInterval = 2, 15*time.Second
	})
	ctx := context.Background()
	shape := Shape{MemoryBytes: 10 * gib}
	_, err := a.Snapshot(ctx)
	require.NoError(t, err)
	cached, err := a.Fits(ctx, shape, []string{"pve1"})
	require.NoError(t, err)
	require.True(t, cached["pve1"])
	rs.mu.Lock()
	delete(rs.nodeStatus, "pve1")
	rs.mu.Unlock()
	_, ok, err := a.ReserveExistingForPacedStart(ctx, "pve1", shape, 10001, time.Now(), false)
	require.NoError(t, err)
	require.False(t, ok, "unreadable fresh memory must not use the warm cache")
	require.Empty(t, a.res)
}

func TestPacedBootLimitAndSeparateSuccessSamples(t *testing.T) {
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 157*gib, 88)})
	rs.setNodeStatus("pve1", 68*gib, 157*gib)
	a := newAccountant(t, rs, func(o *Options) {
		o.MemoryMode, o.ReserveBytes, o.Refresh = MemoryModeHostAvailable, 24*gib, 15*time.Second
		o.PacedStartLimit, o.PacedStartInterval = 2, 15*time.Second
	})
	ctx := context.Background()
	now := time.Now()
	a.now = func() time.Time { return now }
	generation := now
	shape := Shape{MemoryBytes: 10 * gib}
	doReserve := func(id int) (Reservation, bool) {
		r, ok, err := a.ReserveExistingForPacedStart(ctx, "pve1", shape, id, generation, false)
		require.NoError(t, err)
		return r, ok
	}
	one, ok := doReserve(10001)
	require.True(t, ok)
	two, ok := doReserve(10002)
	require.True(t, ok)
	require.True(t, one.(*reservation).BeginStart())
	require.True(t, two.(*reservation).BeginStart())
	_, ok = doReserve(10003)
	require.False(t, ok, "two unresolved starts, not 44 GiB of wave claims")
	receipt := WithPacedTaskReceipt(ctx, one)
	ReportPacedTask(receipt, "pve1", 10001, "first-upid", false)
	ReportPacedTask(receipt, "pve1", 10001, "wrong-upid", true)
	ReportPacedReady(receipt, "runner-10001")
	require.True(t, one.(*reservation).pacedProof.ready.IsZero(), "wrong/unsuccessful task cannot yield readiness proof")
	ReportPacedTask(receipt, "pve1", 10001, "first-upid", true)
	now = now.Add(2 * time.Second)
	ReportPacedReady(receipt, "runner-10001")
	now = now.Add(5 * time.Second)
	row := guestRow(10001, "pve1", shape.MemoryBytes, 4)
	row["name"], row["uptime"] = "runner-10001", 7
	rs.setRows([]map[string]any{nodeRow("pve1", 157*gib, 88), row})
	_, ok = doReserve(10003)
	require.False(t, ok, "one valid independent sample alone cannot retire the hold")
	now = now.Add(15 * time.Second)
	_, ok = doReserve(10003)
	require.False(t, ok, "fresh host RAM with stale guest uptime cannot retire the hold")
	row["uptime"] = 22
	rs.setRows([]map[string]any{nodeRow("pve1", 157*gib, 88), row})
	_, ok = doReserve(10003)
	require.True(t, ok, "task, ready guest and two fresh matching observations allow the next paced start")
	require.True(t, one.(*reservation).released)
	require.False(t, two.(*reservation).released, "neighbor's unresolved task/guest is uncredited")
}

func TestPacedStartIgnoresUnrelatedNegativeGuestUsageNotHostMemory(t *testing.T) {
	row := guestRow(2020, "pve1", 4*gib, 2)
	row["mem"] = -777469952
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16), row})
	rs.setNodeStatus("pve1", 24*gib, 32*gib)
	a := newAccountant(t, rs, func(o *Options) {
		o.MemoryMode, o.ReserveBytes, o.Refresh = MemoryModeHostAvailable, 4*gib, time.Minute
		o.PacedStartLimit, o.PacedStartInterval = 2, 15*time.Second
	})
	ctx := context.Background()
	shape := Shape{MemoryBytes: 10 * gib}
	claim, ok, err := a.ReserveExistingForPacedStart(ctx, "pve1", shape, 10001, time.Now(), false)
	require.NoError(t, err)
	require.True(t, ok)
	claim.Release()
	rs.mu.Lock()
	rs.nodeStatus["pve1"] = map[string]any{"memory": map[string]any{"available": -1, "used": 8 * gib, "total": 32 * gib}}
	rs.mu.Unlock()
	_, ok, err = a.ReserveExistingForPacedStart(ctx, "pve1", shape, 10002, time.Now(), false)
	require.NoError(t, err)
	require.False(t, ok, "malformed host available memory cannot fall back to another measure")
}

func TestPacedRunningRestartUsesDirectHostFloorNotSiblingLaunchCharges(t *testing.T) {
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 4)})
	rs.setNodeStatus("pve1", 10*gib, 32*gib)
	rs.setNodeCPU("pve1", 0.25)
	a := newAccountant(t, rs, func(o *Options) {
		o.MemoryMode, o.ReserveBytes, o.Refresh = MemoryModeHostAvailable, 4*gib, time.Minute
		o.PacedStartLimit, o.PacedStartInterval = 2, 15*time.Second
		o.CPUMode, o.CPUUsageLimit, o.CPULaunchVCPUs = CPUModeHostUsage, .80, 3
	})
	ctx := context.Background()
	a.res[1] = &reservation{id: 1, node: "pve1", vmid: 10002, shape: Shape{MemoryBytes: 10 * gib, VCPUs: 3}, expires: time.Now().Add(time.Minute)}
	claim, ok, err := a.ReservePacedRestart(ctx, "pve1", 10001, time.Now())
	require.NoError(t, err)
	require.True(t, ok, "unrelated launch holds must not block a running guest within actual host floors")
	require.Nil(t, claim, "the running guest incurs no new shape")
	rs.setNodeStatus("pve1", 3*gib, 32*gib)
	rs.setNodeCPU("pve1", .25)
	_, ok, err = a.ReservePacedRestart(ctx, "pve1", 10001, time.Now())
	require.NoError(t, err)
	require.False(t, ok, "below the real host floor a running restart does not issue")
}

func TestPacedSubmittedHoldNeedsEvidenceNotTimeout(t *testing.T) {
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
	rs.setNodeStatus("pve1", 31*gib, 32*gib)
	a := newAccountant(t, rs, func(o *Options) {
		o.MemoryMode, o.ReserveBytes, o.Refresh = MemoryModeHostAvailable, 4*gib, time.Minute
		o.PacedStartLimit, o.PacedStartInterval = 2, 15*time.Second
	})
	ctx := context.Background()
	shape := Shape{MemoryBytes: 10 * gib}
	gen := time.Now()
	r, ok, err := a.ReserveExistingForPacedStart(ctx, "pve1", shape, 10001, gen, false)
	require.NoError(t, err)
	require.True(t, ok)
	started := r.(*reservation)
	originalExpiry := started.expires
	require.True(t, started.BeginStart())
	require.Equal(t, originalExpiry, started.expires, "submission does not renew a timer")
	pending, ok, err := a.ReserveExistingForPacedStart(ctx, "pve1", shape, 10002, gen, false)
	require.NoError(t, err)
	require.True(t, ok)
	// Simulate passage of the abandoned-pending safety deadline.
	a.mu.Lock()
	started.expires = gen.Add(-time.Second)
	pending.(*reservation).expires = gen.Add(-time.Second)
	a.mu.Unlock()
	third, ok, err := a.ReserveExistingForPacedStart(ctx, "pve1", shape, 10003, gen, false)
	require.NoError(t, err)
	require.True(t, ok, "only the abandoned pending slot expires")
	defer third.Release()
	require.False(t, started.released, "missing task/ready evidence must retain the submitted hold")
	require.True(t, pending.(*reservation).released)
	a.ReservationsBeforeDestroy("pve1", 10001)()
	require.True(t, started.released, "confirmed destroy frees this generation promptly")
}

func TestHostAdmissionRequiresEvidenceToEndRestartRecovery(t *testing.T) {
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
	rs.setNodeStatus("pve1", 31*gib, 32*gib)
	a := newAccountant(t, rs, func(o *Options) {
		o.MemoryMode, o.ReserveBytes, o.Refresh = MemoryModeHostAvailable, 4*gib, time.Minute
		o.PacedStartLimit, o.PacedStartInterval = 2, 15*time.Second
	})
	a.RequireRecovery(true)
	a.BeginRecovery(0)
	shape := Shape{MemoryBytes: 10 * gib}
	_, ok, err := a.ReserveExistingForPacedStart(t.Context(), "pve1", shape, 10001, time.Now(), false)
	require.NoError(t, err)
	require.False(t, ok, "elapsed time is not recovery proof")
	a.RequireRecovery(false)
	claim, ok, err := a.ReserveExistingForPacedStart(t.Context(), "pve1", shape, 10001, time.Now(), false)
	require.NoError(t, err)
	require.True(t, ok)
	claim.Release()
}

func TestRestartHoldIsExactAndDoesNotCountAsPendingPower(t *testing.T) {
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
	rs.setNodeStatus("pve1", 31*gib, 32*gib)
	a := newAccountant(t, rs, func(o *Options) {
		o.MemoryMode, o.ReserveBytes, o.Refresh = MemoryModeHostAvailable, 4*gib, time.Minute
		o.PacedStartLimit, o.PacedStartInterval = 2, 15*time.Second
	})
	a.RequireRecovery(true)
	gen := time.Now()
	require.NoError(t, a.InstallRestartHolds([]RestartHold{{Node: "pve1", VMID: 10001, Generation: gen, Shape: Shape{MemoryBytes: 4 * gib, VCPUs: 4}}}))
	a.RequireRecovery(false)
	for _, id := range []int{10002, 10003} {
		_, ok, err := a.ReserveExistingForPacedStart(t.Context(), "pve1", Shape{MemoryBytes: 10 * gib}, id, gen, false)
		require.NoError(t, err)
		require.True(t, ok, "unready inherited guest did not consume a launch slot")
	}
	state, err := a.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, uint64(3*gib), state["pve1"].AvailableBytes)
	a.ForgetRestartHold("pve1", 10001, gen.Add(time.Nanosecond))
	state, err = a.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, uint64(3*gib), state["pve1"].AvailableBytes, "replacement identity cannot release charge")
	a.ForgetRestartHold("pve1", 10001, gen)
	state, err = a.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, uint64(7*gib), state["pve1"].AvailableBytes)
}
