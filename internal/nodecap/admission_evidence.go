package nodecap

import (
	"context"
	"log/slog"
	"time"
)

type admissionKey struct{ node, reason string }

// Sample the exact decision snapshot and ledger rather than infer denial
// reasons later from asynchronously scraped gauges. Caller holds a.mu.
func (a *Accountant) logAdmissionDenial(snap *snapshot, node, reason string, shape Shape, memCredit uint64, vcpuCredit int, st NodeState) {
	if a.log == nil || !a.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	now := a.now()
	key := admissionKey{node, reason}
	if last := a.lastAdmissionLog[key]; !last.IsZero() && now.Sub(last) < 10*time.Second {
		return
	}
	if a.lastAdmissionLog == nil {
		a.lastAdmissionLog = make(map[admissionKey]time.Time)
	}
	a.lastAdmissionLog[key] = now
	t := snap.nodes[node]
	var pending, started uint64
	for _, r := range a.res {
		if r.node != node || r.released {
			continue
		}
		if r.started {
			started += r.shape.MemoryBytes
		} else {
			pending += r.shape.MemoryBytes
		}
	}
	var held, freeing, windows uint64
	if a.isHostMode() && !st.MemoryPressured && reason != "restart_recovery" {
		held, freeing = a.holdsAndCreditsLocked(snap, node)
		windows = a.windowsSettledCreditLocked(snap, node)
	}
	a.log.Debug("nodecap: sampled admission refusal",
		"node", node, "reason", reason, "snapshot_age_ms", now.Sub(snap.takenAt).Milliseconds(),
		"host_available_bytes", t.memAvailable, "host_reserve_bytes", a.reserveFor(t.memBytes),
		"admission_available_bytes", st.AvailableBytes, "request_memory_bytes", shape.MemoryBytes,
		"pending_claim_shape_bytes", pending, "started_claim_shape_bytes", started,
		"net_held_bytes", held, "windows_settle_eligible_credit_bytes", windows,
		"ledger_freeing_credit_bytes", freeing, "request_memory_credit_bytes", memCredit,
		"host_cpu_fraction", t.cpuUsage, "admission_cpu_limit", a.vcpuLimit(t.vcpus),
		"admission_committed_cpu", st.CommittedVCPU, "request_cpu", st.LaunchVCPUs(shape),
		"request_cpu_credit", vcpuCredit)
}
