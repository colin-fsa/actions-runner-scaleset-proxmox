package nodecap

import (
	"context"
	"time"
)

// Power task receipts stay in the calling goroutine. A successful
// readiness probe cannot manufacture a receipt (e.g. AlreadyRunning).
type pacedReceiptKey struct{}

type receiptContext struct {
	task     func(string, int, string, bool)
	ready    func(string)
	returned func(string, string)
	done     func()
}

func WithPacedTaskReceipt(ctx context.Context, claim Reservation) context.Context {
	var r *reservation
	switch c := claim.(type) {
	case *reservation:
		r = c
	case borrowedStart:
		r = c.original
	default:
		return ctx
	}
	r.acct.mu.Lock()
	stamp := r.startedAt
	r.acct.mu.Unlock()
	return context.WithValue(ctx, pacedReceiptKey{}, receiptContext{
		task: func(node string, vmid int, upid string, completed bool) {
			r.pacedTask(stamp, node, vmid, upid, completed)
		},
		ready: func(name string) { r.pacedReady(stamp, name) },
		done: func() {
			r.acct.mu.Lock()
			defer r.acct.mu.Unlock()
			if r.paced && !r.released && r.startedAt.Equal(stamp) {
				r.pacedProof.workerDone = true
			}
		},
		returned: func(name, kind string) {
			r.acct.mu.Lock()
			defer r.acct.mu.Unlock()
			if r.paced && !r.released && r.startedAt.Equal(stamp) && name != "" && (kind == "qemu" || kind == "lxc") {
				r.pacedProof.submitName, r.pacedProof.submitKind = name, kind
				r.pacedProof.returned = r.acct.now()
			}
		},
	})
}

func ReportPacedTask(ctx context.Context, node string, vmid int, upid string, completed bool) {
	if evidence, ok := ctx.Value(pacedReceiptKey{}).(receiptContext); ok {
		evidence.task(node, vmid, upid, completed)
	}
}

func ReportPacedReady(ctx context.Context, name string) {
	if evidence, ok := ctx.Value(pacedReceiptKey{}).(receiptContext); ok {
		evidence.ready(name)
	}
}

// ReportPacedSubmissionReturned marks the exact boundary after the
// HTTP call returned, including an ambiguous error or missing UPID.
func ReportPacedSubmissionReturned(ctx context.Context, name, kind string) {
	if evidence, ok := ctx.Value(pacedReceiptKey{}).(receiptContext); ok {
		evidence.returned(name, kind)
	}
}

// ReportPacedWorkerDone enables ambiguous-result recovery only after
// this attempt has no normal boot/readiness worker left to complete it.
func ReportPacedWorkerDone(ctx context.Context) {
	if evidence, ok := ctx.Value(pacedReceiptKey{}).(receiptContext); ok {
		evidence.done()
	}
}

type pacedBootProof struct {
	upid                   string
	completed              bool
	ready                  time.Time
	name                   string
	firstSample            time.Time
	firstBootAt            time.Time
	firstUptime            uint64
	submitName, submitKind string
	returned, probeAt      time.Time
	workerDone             bool
	rechecked              inactiveStartView
}

func (r *reservation) pacedTask(stamp time.Time, node string, vmid int, upid string, completed bool) {
	a := r.acct
	a.mu.Lock()
	defer a.mu.Unlock()
	if !r.paced || r.released || !r.started || !r.startedAt.Equal(stamp) || r.node != node || r.vmid != vmid || upid == "" {
		return
	}
	if !completed && r.pacedProof.upid == "" {
		r.pacedProof.upid = upid
	}
	if completed && r.pacedProof.upid == upid {
		r.pacedProof.completed = true
	}
}

func (r *reservation) pacedReady(stamp time.Time, name string) {
	a := r.acct
	a.mu.Lock()
	defer a.mu.Unlock()
	if !r.paced || r.released || r.generation.IsZero() || !r.startedAt.Equal(stamp) || !r.pacedProof.completed || name == "" {
		return
	}
	if r.pacedProof.ready.IsZero() {
		r.pacedProof.ready, r.pacedProof.name = a.now(), name
	}
}

// pacedBootVisibleLocked retires a boot hold, not a promise to reserve all
// future job RSS. The measured policy retains the normal hard reserve,
// waits for identical boot identity and two independent fresh host reads
// after task success and guest readiness, and never assumes a boot failed
// merely because it is absent from a read.
func (a *Accountant) pacedBootVisibleLocked(snap *snapshot, r *reservation) bool {
	p := &r.pacedProof
	if !r.paced || p.ready.IsZero() {
		return false
	}
	sample := snap.nodes[r.node]
	guest, known := snap.runningGuests[r.vmid]
	if !known || guest.node != r.node || guest.name != p.name || guest.uptime == 0 ||
		!snap.takenAt.After(p.ready) || !sample.memReadAt.After(p.ready) ||
		a.recoveringLocked() || a.stale(snap) ||
		sample.memMissing || a.now().Sub(sample.memReadAt) > a.opts.Refresh ||
		sample.memAvailable < a.reserveFor(sample.memBytes) ||
		(a.cpuUsageMode() && (sample.cpuMissing || sample.vcpus <= 0 ||
			sample.cpuUsage >= float64(a.vcpuLimit(sample.vcpus))/float64(sample.vcpus))) {
		return false
	}
	bootAt := snap.takenAt.Add(-time.Duration(guest.uptime) * time.Second)
	// Bound pvestatd timing lag without accepting a reboot after readiness.
	slop := max(a.opts.Refresh, 15*time.Second)
	if bootAt.Before(r.startedAt.Add(-slop)) || bootAt.After(p.ready) {
		return false
	}
	if p.firstSample.IsZero() {
		p.firstSample, p.firstBootAt, p.firstUptime = sample.memReadAt, bootAt, guest.uptime
		return false
	}
	if sample.memReadAt.Sub(p.firstSample) < max(a.opts.PacedStartInterval, 15*time.Second) {
		return false
	}
	if guest.uptime <= p.firstUptime || bootAt.Before(p.firstBootAt.Add(-slop)) || bootAt.After(p.firstBootAt.Add(slop)) {
		return false
	}
	a.log.Debug("nodecap: observed paced boot; retired start hold", "node", r.node, "vmid", r.vmid,
		"generation", r.generation, "claim", r.id, "upid", p.upid, "memory_bytes", r.shape.MemoryBytes)
	return true
}
