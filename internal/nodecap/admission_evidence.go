package nodecap

import (
	"context"
	"log/slog"
	"slices"
	"time"
)

type admissionKey struct{ node, reason string }

const maxAdmissionClaimsLogged = 12

type admissionClaimEvidence struct {
	ID                  uint64    `json:"id"`
	VMID                int       `json:"vmid"`
	Generation          time.Time `json:"generation,omitzero"`
	ShapeBytes          uint64    `json:"shape_bytes"`
	Phase               string    `json:"phase"`
	AgeMS               int64     `json:"age_ms"`
	RemainingMS         int64     `json:"remaining_ms"`
	StartedAt           time.Time `json:"issued_at,omitzero"`
	SeenInInventory     bool      `json:"seen_in_inventory"`
	InventoryNonstopped bool      `json:"inventory_nonstopped"`
	WindowsOnlineProof  bool      `json:"windows_online_proof"`
}

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
	var claims int
	ids := make([]uint64, 0, maxAdmissionClaimsLogged)
	for id, r := range a.res {
		if r.node != node || r.released {
			continue
		}
		claims++
		if len(ids) < maxAdmissionClaimsLogged || id < ids[len(ids)-1] {
			at, _ := slices.BinarySearch(ids, id)
			ids = slices.Insert(ids, at, id)
			if len(ids) > maxAdmissionClaimsLogged {
				ids = ids[:maxAdmissionClaimsLogged]
			}
		}
		if r.started {
			started += r.shape.MemoryBytes
		} else {
			pending += r.shape.MemoryBytes
		}
	}
	details := make([]admissionClaimEvidence, 0, len(ids))
	for _, id := range ids {
		r := a.res[id]
		phase := "pending"
		if r.started {
			phase = "start_issued_or_issuing"
			if !r.pacedProof.ready.IsZero() {
				phase = "ready_awaiting_fresh_host_observations"
			} else if r.pacedProof.completed {
				phase = "task_succeeded_awaiting_readiness"
			} else if r.pacedProof.upid != "" {
				phase = "task_accepted_not_confirmed"
			}
		} else if r.poweredOff {
			phase = "stopped_clone_waiting_for_inventory"
		}
		_, seen := snap.guestMem[r.vmid]
		details = append(details, admissionClaimEvidence{
			ID: id, VMID: r.vmid, Generation: r.generation, ShapeBytes: r.shape.MemoryBytes, Phase: phase,
			AgeMS: now.Sub(r.created).Milliseconds(), RemainingMS: r.expires.Sub(now).Milliseconds(),
			StartedAt: r.startedAt, SeenInInventory: seen, InventoryNonstopped: snap.guestPowered[r.vmid],
			WindowsOnlineProof: !r.online.last.IsZero(),
		})
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
		"claims", details, "claims_omitted", claims-len(details),
		"net_held_bytes", held, "windows_settle_eligible_credit_bytes", windows,
		"ledger_freeing_credit_bytes", freeing, "request_memory_credit_bytes", memCredit,
		"host_cpu_fraction", t.cpuUsage, "admission_cpu_limit", a.vcpuLimit(t.vcpus),
		"admission_committed_cpu", st.CommittedVCPU, "request_cpu", st.LaunchVCPUs(shape),
		"request_cpu_credit", vcpuCredit)
}
