package nodecap

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPacedActualHostFloorDoesNotSubtractFutureGrowth(t *testing.T) {
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 100*gib, 100)})
	rs.setNodeCPU("pve1", .20)
	a := newAccountant(t, rs, func(o *Options) {
		o.Refresh = time.Minute
		o.MemoryMode = MemoryModeHostAvailable
		o.ReserveFraction = .15
		o.PacedStartLimit = 2
		o.PacedStartInterval = 15 * time.Second
		o.CPUMode = CPUModeHostUsage
		o.CPUUsageLimit = .85
		o.CPULaunchVCPUs = 10
	})
	gen := time.Now()
	for i, tc := range []struct {
		available uint64
		cpu       float64
		want      bool
	}{
		{16 * gib, .84, true}, // even a huge configured allocation is not a physical reading
		{16 * gib, .84, true},
		{16 * gib, .84, true},  // unresolved launches are not readiness slots
		{15 * gib, .20, false}, // exactly 85% used
		{14 * gib, .20, false},
		{16 * gib, .86, false},
		{16 * gib, .20, true},
	} {
		rs.setNodeStatus("pve1", tc.available, 100*gib)
		rs.setNodeCPU("pve1", tc.cpu)
		r, ok, err := a.ReserveExistingForPacedStart(t.Context(), "pve1", Shape{MemoryBytes: 40 * gib, VCPUs: 32}, 10001+i, gen, false)
		require.NoError(t, err)
		require.Equal(t, tc.want, ok, "case %d", i)
		if ok {
			require.True(t, r.(*reservation).BeginStart())
		}
	}
	require.Len(t, a.res, 4, "ownership evidence is retained despite not debiting headroom")
	state, err := a.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, uint64(gib), state["pve1"].AvailableBytes)
}
