package pool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSubmissionWindowRollingBoundaryIndependentOfReadiness(t *testing.T) {
	now := time.Unix(100, 0)
	var w submissionWindow
	require.Zero(t, w.delay(now, 2, 15*time.Second))
	w.submitted(now)
	require.Zero(t, w.delay(now.Add(time.Second), 2, 15*time.Second))
	w.submitted(now.Add(time.Second))
	require.Equal(t, 14*time.Second, w.delay(now.Add(time.Second), 2, 15*time.Second))
	require.Equal(t, time.Nanosecond, w.delay(now.Add(15*time.Second-time.Nanosecond), 2, 15*time.Second))
	require.Zero(t, w.delay(now.Add(15*time.Second), 2, 15*time.Second))
	w.submitted(now.Add(15 * time.Second))
	require.Equal(t, time.Second, w.delay(now.Add(15*time.Second), 2, 15*time.Second))
	// Nothing ever reported task completion/readiness to open this window.
}

func TestPacedColdConcurrentSubmissionsAndCancelledWait(t *testing.T) {
	b := NewHostBootBudget(2, time.Hour, 0)
	var submitted atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			require.NoError(t, b.paceAction(t.Context(), "pve1", func(mark func()) error { mark(); submitted.Add(1); return nil }))
		})
	}
	wg.Wait()
	require.EqualValues(t, 2, submitted.Load())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- b.paceAction(ctx, "pve1", func(mark func()) error { mark(); submitted.Add(1); return nil })
	}()
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.EqualValues(t, 2, submitted.Load())
	// A cancelled/refused request cannot poison another node or the warm path.
	require.NoError(t, b.warmAction(t.Context(), "pve1", nil))
	require.NoError(t, b.paceAction(t.Context(), "pve2", nil))
}

func TestRefusedColdRequestDoesNotSpendWindowAmbiguousSubmissionDoes(t *testing.T) {
	b := NewHostBootBudget(1, time.Hour, 0)
	failure := errors.New("not admitted")
	require.ErrorIs(t, b.paceAction(t.Context(), "pve1", func(func()) error { return failure }), failure)
	require.ErrorIs(t, b.paceAction(t.Context(), "pve1", func(mark func()) error { mark(); return failure }), failure)
	n := b.node("pve1")
	n.pace.Lock()
	defer n.pace.Unlock()
	require.Len(t, n.starts.times, 1)
	require.Positive(t, n.starts.delay(time.Now(), 1, time.Hour))
}
