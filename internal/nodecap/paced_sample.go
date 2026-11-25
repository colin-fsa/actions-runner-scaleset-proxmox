package nodecap

import (
	"context"
	"errors"
	"time"
)

var (
	ErrPacedStartSlots    = errors.New("unsettled start slots full")
	ErrPacedStartCapacity = errors.New("actual host capacity refused start")
	ErrPacedStartRead     = errors.New("fresh Proxmox admission read failed")
	ErrPacedStartStale    = errors.New("prepared start sample expired or superseded")
)

// PacedStartSample is a direct observation, not a reservation or a promise
// to start. Concurrent callers may share it; Reserve charges the ledger
// atomically and rejects expired/superseded observations at the POST boundary.
type PacedStartSample interface {
	Reserve(context.Context, string, Shape, int, time.Time, bool) (Reservation, bool, error)
}
type pacedSample struct {
	a    *Accountant
	snap *snapshot
}

// SamplePacedStart does all Proxmox I/O outside the shared power POST lock.
// Each probe has a bounded lifetime; no stale/cache fallback permits a POST.
func (a *Accountant) SamplePacedStart(ctx context.Context, node string) (PacedStartSample, error) {
	readCtx, cancel := context.WithTimeout(ctx, min(a.opts.Refresh, fetchTimeout, time.Second))
	defer cancel()
	snap, err := a.fetch(readCtx)
	if err != nil {
		return nil, errors.Join(ErrPacedStartRead, err)
	}
	totals, known := snap.nodes[node]
	if !known || totals.memMissing || (a.cpuUsageMode() && totals.cpuMissing) {
		return nil, ErrPacedStartRead
	}
	a.recheckInactiveStarts(readCtx, node, snap)
	if err := readCtx.Err(); err != nil {
		return nil, errors.Join(ErrPacedStartRead, err)
	}
	return pacedSample{a: a, snap: snap}, nil
}
func (p pacedSample) Reserve(ctx context.Context, node string, shape Shape, vmid int, generation time.Time, promotion bool) (Reservation, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	r, ok, err := p.a.reserveExistingDetailed(p.snap, node, shape, vmid, generation, promotion, true, true)
	if !ok && err == nil {
		err = ErrPacedStartCapacity
	}
	return r, ok, err
}
