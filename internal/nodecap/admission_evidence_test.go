package nodecap

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAdmissionDenialEvidenceUsesDecisionSnapshotAndSamples(t *testing.T) {
	var log bytes.Buffer
	now := time.Now()
	acct := &Accountant{
		opts: Options{MemoryMode: MemoryModeHostAvailable, Refresh: time.Minute, ReserveBytes: gib},
		log:  slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		now:  func() time.Time { return now },
		res:  map[uint64]*reservation{1: {node: "pve1", started: true, shape: Shape{MemoryBytes: 4 * gib}}},
	}
	snap := &snapshot{nodes: map[string]nodeTotals{"pve1": {memBytes: 16 * gib, memAvailable: 6 * gib, memReadAt: now, vcpus: 8}}, takenAt: now}
	require.False(t, acct.fitsLocked(snap, "pve1", Shape{MemoryBytes: 2 * gib}, nil))
	require.False(t, acct.fitsLocked(snap, "pve1", Shape{MemoryBytes: 2 * gib}, nil))
	require.Equal(t, 1, strings.Count(log.String(), "nodecap: sampled admission refusal"))
	require.Contains(t, log.String(), "reason=memory")
	require.Contains(t, log.String(), "admission_available_bytes=1073741824")
	require.Contains(t, log.String(), "net_held_bytes=4294967296")
	require.Contains(t, log.String(), "started_claim_shape_bytes=4294967296")
	require.True(t, acct.fitsLocked(snap, "pve1", Shape{MemoryBytes: gib}, nil))
	now = now.Add(11 * time.Second)
	require.False(t, acct.fitsLocked(snap, "pve1", Shape{MemoryBytes: 2 * gib}, nil))
	require.Equal(t, 2, strings.Count(log.String(), "nodecap: sampled admission refusal"))
}
