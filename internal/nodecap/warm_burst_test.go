package nodecap

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConcurrentWarmBurstSharesAtomicStartAndMemoryLimits(t *testing.T) {
	for _, tc := range []struct {
		name      string
		available uint64
		want      int
	}{{"start_limit", 60 * gib, 2}, {"memory_limit", 14 * gib, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 64*gib, 32)})
			rs.setNodeStatus("pve1", tc.available, 64*gib)
			a := newAccountant(t, rs, func(o *Options) {
				o.Refresh = time.Minute
				o.MemoryMode = MemoryModeHostAvailable
				o.ReserveBytes = 4 * gib
				o.PacedStartLimit = 2
				o.PacedStartInterval = 15 * time.Second
			})
			gen := time.Now()
			var wg sync.WaitGroup
			var mu sync.Mutex
			var claims []Reservation
			start := make(chan struct{})
			for i := range 32 {
				wg.Go(func() {
					<-start
					r, ok, err := a.ReserveExistingForPacedStart(t.Context(), "pve1", Shape{MemoryBytes: 10 * gib}, 10001+i, gen, true)
					if err != nil {
						t.Error(err)
					}
					if ok {
						mu.Lock()
						claims = append(claims, r)
						mu.Unlock()
					}
				})
			}
			close(start)
			wg.Wait()
			require.Len(t, claims, tc.want, "every profile shares the same outstanding-start and memory ledger")
			for _, r := range claims {
				r.Release()
			}
			r, ok, err := a.ReserveExistingForPacedStart(t.Context(), "pve1", Shape{MemoryBytes: 10 * gib}, 10100, gen, true)
			require.NoError(t, err)
			require.True(t, ok, "pre-submission failure returns its capacity")
			r.Release()
		})
	}
}
