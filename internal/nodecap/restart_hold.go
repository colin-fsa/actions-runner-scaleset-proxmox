package nodecap

import (
	"errors"
	"time"
)

// RestartHold covers a stable inherited process whose guest readiness
// cannot be established. It withholds capacity but is not an in-flight
// power action and must not consume a paced launch slot.
type RestartHold struct {
	Node       string
	VMID       int
	Generation time.Time
	Shape      Shape
	// HostObservedAt is set only by startup's second exact running
	// process + fresh, valid post-inventory host read. Reservation and
	// row quarantine are independent decisions.
	HostObservedAt time.Time
}

func (a *Accountant) InstallRestartHolds(holds []RestartHold) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.verificationPending {
		return errors.New("restart holds require closed recovery admission")
	}
	seen := make(map[int]struct{}, len(holds))
	now := a.now()
	for _, h := range holds {
		if _, duplicate := seen[h.VMID]; duplicate {
			return errors.New("duplicate inherited guest capacity identity")
		}
		seen[h.VMID] = struct{}{}
		if h.Node == "" || h.VMID <= 0 || h.Generation.IsZero() || h.Shape.MemoryBytes == 0 || h.Shape.VCPUs <= 0 {
			return errors.New("invalid inherited guest capacity identity")
		}
		if a.SupportsPacedStart() && !h.HostObservedAt.IsZero() &&
			(h.HostObservedAt.After(now) || now.Sub(h.HostObservedAt) > a.opts.Refresh) {
			return errors.New("inherited guest host visibility is stale; repeat restart proof")
		}
		for _, r := range a.res {
			if r.node == h.Node && r.vmid == h.VMID && (!r.inherited || !r.generation.Equal(h.Generation) || r.shape != h.Shape) {
				return errors.New("inherited guest conflicts with existing reservation")
			}
		}
	}
	for _, h := range holds {
		if a.SupportsPacedStart() && !h.HostObservedAt.IsZero() {
			a.recordPacedHostObservationLocked(h.Node, h.HostObservedAt)
			a.log.Debug("nodecap: inherited guest covered by direct host visibility; no extra hold",
				"node", h.Node, "vmid", h.VMID, "generation", h.Generation, "host_sample", h.HostObservedAt)
			continue
		}
		var present bool
		for _, r := range a.res {
			if r.node == h.Node && r.vmid == h.VMID && r.generation.Equal(h.Generation) && r.inherited {
				present = true
				break
			}
		}
		if present {
			continue
		}
		a.nextRes++
		a.res[a.nextRes] = &reservation{id: a.nextRes, node: h.Node, vmid: h.VMID, generation: h.Generation, shape: h.Shape,
			created: now, startedAt: now, started: true, inherited: true, acct: a}
		a.log.Warn("nodecap: inherited guest readiness unproved; preserving footprint",
			"node", h.Node, "vmid", h.VMID, "generation", h.Generation, "memory_bytes", h.Shape.MemoryBytes)
	}
	return nil
}

// ForgetRestartHold clears only an exact inherited generation whose
// removal is confirmed by the existing cleanup path.
func (a *Accountant) ForgetRestartHold(node string, vmid int, generation time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, r := range a.res {
		if r.inherited && r.node == node && r.vmid == vmid && r.generation.Equal(generation) {
			r.released = true
			delete(a.res, id)
		}
	}
}

// Caller holds a.mu. Prevent a paced POST using a concurrent read taken
// before a launch debit was retired against newer host evidence.
func (a *Accountant) recordPacedHostObservationLocked(node string, observed time.Time) {
	if a.lastPacedHostObservation == nil {
		a.lastPacedHostObservation = make(map[string]time.Time)
	}
	if observed.After(a.lastPacedHostObservation[node]) {
		a.lastPacedHostObservation[node] = observed
	}
}
