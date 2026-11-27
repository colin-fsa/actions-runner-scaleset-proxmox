package nodecap

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPreparedPacedSampleIsFreshWithoutReadinessSlots(t *testing.T) {
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 64*gib, 32)})
	rs.setNodeStatus("pve1", 60*gib, 64*gib)
	a := newAccountant(t, rs, func(o *Options) {
		o.Refresh = time.Minute
		o.MemoryMode = MemoryModeHostAvailable
		o.ReserveBytes = 4 * gib
		o.PacedStartLimit = 2
		o.PacedStartInterval = 15 * time.Second
	})
	sample, err := a.SamplePacedStart(t.Context(), "pve1")
	require.NoError(t, err)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var claims []Reservation
	gen := time.Now()
	for i := range 32 {
		wg.Go(func() {
			r, ok, err := sample.Reserve(t.Context(), "pve1", Shape{MemoryBytes: 10 * gib}, 10001+i, gen, false)
			require.NoError(t, err)
			if ok {
				mu.Lock()
				claims = append(claims, r)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	require.Len(t, claims, 32)
	for _, r := range claims {
		r.Release()
	}
	now := a.now()
	a.now = func() time.Time { return now.Add(2 * time.Second) }
	_, ok, err := sample.Reserve(t.Context(), "pve1", Shape{MemoryBytes: 10 * gib}, 10100, gen, false)
	require.False(t, ok)
	require.ErrorIs(t, err, ErrPacedStartStale)
	require.Empty(t, a.res, "expired sample cannot buy a POST claim")
}

func TestPreparedSampleDistinguishesCapacityAndRejectsOldHostEvidence(t *testing.T) {
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 64*gib, 32)})
	rs.setNodeStatus("pve1", 4*gib, 64*gib)
	a := newAccountant(t, rs, func(o *Options) {
		o.Refresh = time.Minute
		o.MemoryMode = MemoryModeHostAvailable
		o.ReserveBytes = 4 * gib
		o.PacedStartLimit = 2
		o.PacedStartInterval = 15 * time.Second
	})
	sample, err := a.SamplePacedStart(t.Context(), "pve1")
	require.NoError(t, err)
	_, ok, err := sample.Reserve(t.Context(), "pve1", Shape{MemoryBytes: 10 * gib}, 10001, time.Now(), false)
	require.False(t, ok)
	require.ErrorIs(t, err, ErrPacedStartCapacity)
	a.mu.Lock()
	a.recordPacedHostObservationLocked("pve1", a.now().Add(time.Millisecond))
	a.mu.Unlock()
	_, ok, err = sample.Reserve(t.Context(), "pve1", Shape{MemoryBytes: gib}, 10002, time.Now(), true)
	require.False(t, ok)
	require.ErrorIs(t, err, ErrPacedStartStale, "another observation retired credit after this sample")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, ok, err = sample.Reserve(ctx, "pve1", Shape{MemoryBytes: gib}, 10002, time.Now(), true)
	require.False(t, ok)
	require.ErrorIs(t, err, context.Canceled)
}

func TestPreparedSampleCannotBypassRecoveryFence(t *testing.T) {
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 64*gib, 32)})
	rs.setNodeStatus("pve1", 60*gib, 64*gib)
	a := newAccountant(t, rs, func(o *Options) {
		o.Refresh = time.Minute
		o.MemoryMode = MemoryModeHostAvailable
		o.ReserveBytes = 4 * gib
		o.PacedStartLimit = 2
		o.PacedStartInterval = 15 * time.Second
	})
	sample, err := a.SamplePacedStart(t.Context(), "pve1")
	require.NoError(t, err)
	a.RequireRecovery(true)
	_, ok, err := sample.Reserve(t.Context(), "pve1", Shape{MemoryBytes: gib}, 10001, time.Now(), true)
	require.False(t, ok)
	require.ErrorIs(t, err, ErrPacedStartCapacity)
	require.Empty(t, a.res)
}
