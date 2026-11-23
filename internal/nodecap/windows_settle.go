package nodecap

import "time"

const windowsEarlyReleasePerNode = 16 << 30

type qemuIdentity struct {
	node   string
	name   string
	uptime uint64
}

type windowsOnlineProof struct {
	name        string
	delivered   time.Time
	first, last time.Time
}

// ObserveWindowsOnline attaches affirmative post-delivery proof to exactly
// one issued start of this owned generation. No claim is removed: a stale
// observation or host sample restores the original hold.
func (a *Accountant) ObserveWindowsOnline(node, name string, vmid int, generation, delivered, observed time.Time) {
	if a.opts.WindowsOnlineSettleAfter <= 0 || !a.isHostMode() ||
		name == "" || generation.IsZero() || delivered.IsZero() || !delivered.Before(observed) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if observed.After(now) {
		return
	}
	var selected *reservation
	for _, r := range a.res {
		if r.node == node && r.vmid == vmid && r.generation.Equal(generation) &&
			r.started && !r.startedAt.IsZero() &&
			!r.poweredOff && len(r.freeing) == 0 && !r.released && now.Before(r.expires) &&
			(selected == nil || r.startedAt.After(selected.startedAt) ||
				(r.startedAt.Equal(selected.startedAt) && r.id > selected.id)) {
			selected = r
		}
	}
	if selected == nil {
		return
	}
	for _, r := range a.res {
		if r != selected && r.node == node && r.vmid == vmid && r.generation.Equal(generation) {
			r.online = windowsOnlineProof{}
		}
	}
	if selected.startedAt.After(delivered) {
		return
	}
	p := &selected.online
	if observed.Before(p.last) || delivered.Before(p.delivered) {
		return
	}
	if p.first.IsZero() || !delivered.Equal(p.delivered) || observed.Sub(p.last) > a.windowsProofAge() {
		*p = windowsOnlineProof{name: name, delivered: delivered, first: observed}
		a.log.Debug("nodecap: Windows runner eligible for post-online settle", "node", node, "vmid", vmid,
			"after", a.opts.WindowsOnlineSettleAfter)
	}
	p.last = observed
}

// ForgetWindowsOnline revokes ambiguous or absent GitHub proof immediately.
func (a *Accountant) ForgetWindowsOnline(node string, vmid int, generation, observed time.Time) {
	if a.opts.WindowsOnlineSettleAfter <= 0 || !a.isHostMode() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.res {
		if r.node == node && r.vmid == vmid && r.generation.Equal(generation) && !observed.Before(r.online.last) {
			r.online = windowsOnlineProof{}
		}
	}
}

func (a *Accountant) windowsProofAge() time.Duration {
	return max(30*time.Second, a.opts.WindowsOnlineFreshness)
}

// windowsSettledCreditLocked relaxes a future-growth cushion, not measured
// RAM. It must never increase admission on an unavailable, stale, or
// already-pressured host. Caller holds a.mu and has swept original expiry.
func (a *Accountant) windowsSettledCreditLocked(snap *snapshot, node string) uint64 {
	if a.opts.WindowsOnlineSettleAfter <= 0 {
		return 0
	}
	totals, known := snap.nodes[node]
	now := a.now()
	if !known || totals.memMissing || a.stale(snap) || totals.memReadAt.IsZero() ||
		now.Before(a.recoveryUntil) || now.Sub(totals.memReadAt) > a.opts.Refresh ||
		totals.memAvailable < a.reserveFor(totals.memBytes) {
		return 0
	}
	if a.cpuUsageMode() && (totals.cpuMissing || totals.cpuUsage >=
		float64(a.vcpuLimit(totals.vcpus))/float64(max(1, totals.vcpus))) {
		return 0
	}
	var credit uint64
	for _, r := range a.res {
		p := r.online
		if r.node != node || p.first.IsZero() || !p.last.After(p.first) ||
			now.Sub(p.last) > a.windowsProofAge() ||
			snap.takenAt.Before(p.first.Add(a.opts.WindowsOnlineSettleAfter)) ||
			totals.memReadAt.Before(p.first.Add(a.opts.WindowsOnlineSettleAfter)) ||
			!a.sameWindowsStart(snap, r) {
			continue
		}
		credit += uint64(float64(r.shape.MemoryBytes) * a.memOvercommit())
		if credit >= windowsEarlyReleasePerNode {
			return windowsEarlyReleasePerNode
		}
	}
	return credit
}

// The QEMU boot occurred between issuing the start and successful JIT.
// Allow the pvestatd reporting lag, but refuse absent/zero uptime or a
// process apparently rebooted outside the original start window.
func (a *Accountant) sameWindowsStart(snap *snapshot, r *reservation) bool {
	q := snap.qemuRunning[r.vmid]
	if q.node != r.node || q.name != r.online.name || q.uptime == 0 {
		return false
	}
	slop := max(a.opts.Refresh, 20*time.Second)
	youngest := max(0, snap.takenAt.Sub(r.online.delivered)-slop)
	oldest := snap.takenAt.Sub(r.startedAt) + slop
	return oldest >= 0 && q.uptime >= uint64(youngest/time.Second) &&
		q.uptime <= uint64(oldest/time.Second)
}
