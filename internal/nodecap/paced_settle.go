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
	upid                    string
	completed               bool
	completedAt             time.Time
	ready                   time.Time
	name                    string
	submitName, submitKind  string
	returned, probeAt       time.Time
	workerDone, slotSettled bool
	rechecked               inactiveStartView
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
	if completed && r.pacedProof.upid == upid && !r.pacedProof.completed {
		r.pacedProof.completed = true
		r.pacedProof.completedAt = a.now()
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
