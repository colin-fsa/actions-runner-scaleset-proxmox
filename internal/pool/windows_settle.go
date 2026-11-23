package pool

import (
	"errors"
	"time"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/store"
)

// RecordWindowsJITDelivery records only a successful inject, never the
// JITPending clearing path (which also runs on failures).
func (m *manager) RecordWindowsJITDelivery(vm *VM, runnerID int64) {
	if !m.cfg.ObserveWindowsOnline || vm == nil || vm.CreatedAt.IsZero() ||
		vm.AssignedAt.IsZero() || runnerID <= 0 || !m.isWindowsQEMUProfile(vm.Profile) {
		return
	}
	_, err := m.store.UpdateIf(vm.VMID, func(v *store.VM) bool {
		return (v.State == store.StateAssigned || v.State == store.StateRunning) &&
			!v.AdoptionProtected && v.JITPending && v.RunnerID == runnerID &&
			v.Name == vm.Name && v.Node == vm.Node && v.Profile == vm.Profile &&
			v.CreatedAt.Equal(vm.CreatedAt) && v.JITAssignedAt.Equal(vm.AssignedAt)
	}, func(v *store.VM) {
		v.WindowsJITAt = time.Now()
		v.WindowsJITRunnerID = runnerID
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		m.log.Debug("windows settle: failed to record successful JIT delivery", "vmid", vm.VMID, "err", err)
	}
}

func (m *manager) WindowsOnlineSettleEnabled() bool { return m.cfg.ObserveWindowsOnline }

// ObserveWindowsRunner connects a complete, affirmative GitHub poll to
// the exact current row and its independently recorded successful JIT.
// Missing or ambiguous observations restore the full original hold.
func (m *manager) ObserveWindowsRunner(row RowSnapshot, gr RunnerInfo, pollStarted time.Time) {
	if !m.cfg.ObserveWindowsOnline || !m.isWindowsQEMUProfile(row.Profile) {
		return
	}
	acct, ok := m.cfg.Capacity.(interface {
		ObserveWindowsOnline(node, name string, vmid int, generation, delivered, observed time.Time)
		ForgetWindowsOnline(node string, vmid int, generation, observed time.Time)
	})
	if !ok {
		return
	}
	v, err := m.store.Get(row.VMID)
	if err != nil || !v.CreatedAt.Equal(row.CreatedAt) ||
		v.Name != row.Name || v.Node != row.Node || v.Profile != row.Profile {
		return
	}
	if row.JITPending || row.AdoptionProtected || v.JITPending || v.AdoptionProtected ||
		(v.State != store.StateAssigned && v.State != store.StateRunning) ||
		v.WindowsJITRunnerID <= 0 || v.WindowsJITAt.IsZero() || gr.Unknown ||
		!gr.Online || gr.ID != v.WindowsJITRunnerID || gr.ID != v.RunnerID || gr.ID != row.RunnerID {
		acct.ForgetWindowsOnline(v.Node, v.VMID, v.CreatedAt, pollStarted)
		return
	}
	if !v.WindowsJITAt.Before(pollStarted) {
		return
	}
	acct.ObserveWindowsOnline(v.Node, v.Name, v.VMID, v.CreatedAt, v.WindowsJITAt, pollStarted)
}

func (m *manager) isWindowsQEMUProfile(profile string) bool {
	ps := m.profileOf(profile)
	return ps != nil && ps.settings.GuestOS == "windows" &&
		(ps.settings.GuestType == "" || ps.settings.GuestType == config.GuestTypeQEMU)
}
