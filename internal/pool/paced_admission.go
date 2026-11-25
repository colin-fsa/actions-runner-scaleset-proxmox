package pool

import (
	"context"
	"errors"
	"time"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/nodecap"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/store"
)

type pacedStartAdmitter interface {
	SupportsPacedStart() bool
	ReserveExistingForPacedStart(context.Context, string, nodecap.Shape, int, time.Time, bool) (nodecap.Reservation, bool, error)
}

var errStartCapacityDeferred = errors.New("fresh node sample did not admit the start")
var errColdStartCapacityBusy = errors.New("cold start waiting for fresh capacity")

func (m *manager) pacedStartAdmitter() pacedStartAdmitter {
	a, ok := m.cfg.Capacity.(pacedStartAdmitter)
	// Preserve parked eviction claims, unbounded and unpaced configurations.
	b := m.cfg.WindowsBootBudget
	if !ok || !a.SupportsPacedStart() || m.cfg.EvictIdleForDemand || b == nil || b.limit <= 0 || b.interval <= 0 {
		return nil
	}
	return a
}

func (m *manager) currentBootWorker(row *store.VM) bool {
	current, err := m.store.Get(row.VMID)
	return err == nil && current.State == store.StateBooting && current.CreatedAt.Equal(row.CreatedAt) &&
		current.StartWorkerDeadline.Equal(row.StartWorkerDeadline) && current.Node == row.Node &&
		current.Name == row.Name && current.Profile == row.Profile
}

func (m *manager) startBootWorker(ctx context.Context, vm *provisioner.VM, row *store.VM, profile *profileState, promotion bool, legacy func() bool) (context.Context, bool, error) {
	a := m.pacedStartAdmitter()
	if a == nil {
		err := m.cfg.WindowsBootBudget.paceAdmission(ctx, row.Node, func() error {
			if !legacy() {
				return errors.New("capacity reservation expired before start")
			}
			return nil
		})
		if err != nil {
			return ctx, false, err
		}
		return ctx, true, m.prov.Start(ctx, vm)
	}
	powerCtx := ctx
	issued := false
	gate := func(ctx context.Context, submit func(context.Context) error) error {
		dispatch := m.cfg.WindowsBootBudget.paceAction
		if promotion {
			dispatch = m.cfg.WindowsBootBudget.warmAction
		}
		return dispatch(ctx, row.Node, func(mark func()) error {
			if profile == nil || !m.currentBootWorker(row) {
				return errStartCapacityDeferred
			}
			r, ok, err := a.ReserveExistingForPacedStart(ctx, row.Node, shapeOf(profile.settings), row.VMID, row.CreatedAt, promotion)
			if err != nil || !ok {
				if promotion {
					m.recordPromotionDeferral(profile, row.Node)
				} else {
					m.recordCapacityDeferral(profile, row.Node, store.PoolKindHot)
				}
				m.log.Debug("start: fresh capacity deferred", "vmid", row.VMID, "node", row.Node, "err", err)
				if !promotion {
					return errColdStartCapacityBusy
				}
				return errStartCapacityDeferred
			}
			claim := claimFor(r)
			if ctx.Err() != nil || !m.currentBootWorker(row) || !claim.beginStart() {
				claim.release()
				return errStartCapacityDeferred
			}
			powerCtx = nodecap.WithPacedTaskReceipt(ctx, r)
			m.log.Debug("start: shape granted before Proxmox request", "vmid", row.VMID, "node", row.Node,
				"generation", row.CreatedAt, "memory_bytes", shapeOf(profile.settings).MemoryBytes)
			issued = true
			mark()
			kind := vm.GuestType
			if kind == "" {
				kind = profile.settings.GuestType
			}
			if kind == "" {
				kind = "qemu"
			}
			defer nodecap.ReportPacedSubmissionReturned(powerCtx, row.Name, kind)
			return submit(powerCtx)
		})
	}
	var err error
	if p, ok := m.prov.(interface {
		StartWithSubmission(context.Context, *provisioner.VM, provisioner.StartSubmission) error
	}); ok {
		err = p.StartWithSubmission(ctx, vm, gate)
	} else {
		// Injected providers without a split submit/wait seam still pass
		// through the gate, conservatively holding it through Start.
		err = gate(ctx, func(ctx context.Context) error { return m.prov.Start(ctx, vm) })
	}
	return powerCtx, issued, err
}

// keepDeferredStartWarm is reachable only before any start call: this
// stopped, fully configured VM may be used by the next reconcile instead.
func (m *manager) keepDeferredStartWarm(row *store.VM) {
	ok, err := m.store.UpdateIf(row.VMID, func(v *store.VM) bool {
		return v.State == store.StateBooting && v.CreatedAt.Equal(row.CreatedAt) &&
			v.StartWorkerDeadline.Equal(row.StartWorkerDeadline)
	}, func(v *store.VM) {
		v.State, v.PoolKind, v.StateSince = store.StateWarm, store.PoolKindWarm, time.Now()
	})
	if err != nil || !ok {
		m.log.Debug("start: refused to return changed row to warm", "vmid", row.VMID, "err", err)
	}
}
