package nodecap

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

type inactiveStartView struct {
	at, hostAt time.Time
	status     string
	pid        int64
	uptime     int64
	task       string
	host       liveStatus
	visible    bool
}

type inactiveStartTarget struct {
	id                           uint64
	vmid                         int
	node, name, kind, upid       string
	attempt, returned, completed time.Time
	probeAt                      time.Time
	allowFallback                bool
}

type inactiveTask struct {
	UPID   string `json:"upid"`
	Node   string `json:"node"`
	Kind   string `json:"type"`
	ID     string `json:"id"`
	Start  int64  `json:"starttime"`
	End    int64  `json:"endtime"`
	Status string `json:"status"`
	Exit   string `json:"exitstatus"`
}

// recheckInactiveStarts settles known successful launches from direct guest and
// host evidence, independently of guest-agent/GitHub readiness. The same
// observations free only a power slot, not RAM, for an ambiguous attempt.
func (a *Accountant) recheckInactiveStarts(ctx context.Context, node string, snap *snapshot) {
	owner := a.opts.PacedTaskOwner
	if _, _, ok := strings.Cut(owner, "!"); !ok {
		return
	}
	now := a.now()
	var pending []inactiveStartTarget
	a.mu.Lock()
	a.sweepLocked(snap)
	nodeView, known := snap.nodes[node]
	if known && !nodeView.memMissing && !a.recoveringLocked() && !a.stale(snap) {
		var eligible []*reservation
		for _, r := range a.res {
			p := &r.pacedProof
			if r.node != node || !r.paced || !r.started || r.inherited || r.generation.IsZero() ||
				p.returned.IsZero() || p.submitName == "" ||
				(!p.completed && (!p.workerDone || now.Sub(p.returned) < a.opts.PacedStartInterval)) ||
				(!p.probeAt.IsZero() && now.Sub(p.probeAt) < 5*time.Second) {
				continue
			}
			eligible = append(eligible, r)
		}
		slices.SortFunc(eligible, func(x, y *reservation) int { return x.pacedProof.probeAt.Compare(y.pacedProof.probeAt) })
		for _, r := range eligible[:min(len(eligible), a.opts.PacedStartLimit)] {
			p := &r.pacedProof
			p.probeAt = now
			pending = append(pending, inactiveStartTarget{id: r.id, vmid: r.vmid, node: r.node, name: p.submitName, kind: p.submitKind,
				upid: p.upid, attempt: r.startedAt, returned: p.returned, completed: p.completedAt, probeAt: now,
				allowFallback: p.workerDone && !p.slotSettled && now.Sub(p.returned) >= a.opts.PacedStartInterval})
		}
	}
	a.mu.Unlock()
	for _, t := range pending {
		view, ok := a.observeInactiveStart(ctx, t, owner)
		visiblySettled := !t.completed.IsZero() && ok
		if !ok && !t.completed.IsZero() && t.allowFallback {
			// A valid receipt is not sufficient if direct power identity has
			// changed. Use the conservative idle fence for the slot only.
			fallback := t
			fallback.completed = time.Time{}
			view, ok = a.observeInactiveStart(ctx, fallback, owner)
		}
		if ctx.Err() != nil {
			return
		}
		a.mu.Lock()
		r := a.res[t.id]
		if r != nil && r.paced && !r.released && !a.recoveringLocked() && r.startedAt.Equal(t.attempt) &&
			r.pacedProof.returned.Equal(t.returned) && r.pacedProof.upid == t.upid && r.pacedProof.completedAt.Equal(t.completed) &&
			r.pacedProof.probeAt.Equal(t.probeAt) {
			prev := r.pacedProof.rechecked
			switch {
			case !ok:
				r.pacedProof.rechecked = inactiveStartView{}
			case prev.at.IsZero() || prev.task != view.task || prev.status != view.status || prev.pid != view.pid || prev.visible != visiblySettled:
				view.visible = visiblySettled
				r.pacedProof.rechecked = view
			case view.hostAt.After(prev.hostAt) && view.at.Sub(prev.at) >= a.opts.PacedStartInterval &&
				(view.status == "stopped" || (view.uptime > prev.uptime &&
					time.Duration(view.uptime-prev.uptime)*time.Second >= view.at.Sub(prev.at)-5*time.Second &&
					time.Duration(view.uptime-prev.uptime)*time.Second <= view.at.Sub(prev.at)+5*time.Second)):
				if visiblySettled {
					// The current decision must use the direct capacity sample
					// taken after the observed guest, not a pre-observation cache.
					totals := snap.nodes[node]
					available, valid := view.host.Memory.AvailableBytes()
					if valid && !view.hostAt.Before(totals.memReadAt) && a.now().Sub(view.hostAt) <= a.opts.Refresh && !a.stale(snap) {
						totals.memAvailable, totals.memReadAt, totals.memMissing = available, view.hostAt, false
						totals.cpuMissing = view.host.CPU == nil
						if cpu := view.host.CPU; cpu != nil {
							totals.cpuUsage = *cpu
							totals.cpuMissing = math.IsNaN(*cpu) || math.IsInf(*cpu, 0) || *cpu < 0 || *cpu > 1
						}
						snap.nodes[node] = totals
						a.recordPacedHostObservationLocked(node, view.hostAt)
						// Older concurrent snapshots cannot admit a paced POST.
						r.released = true
						delete(a.res, r.id)
						a.log.Debug("nodecap: retired launch hold after direct host visibility",
							"node", r.node, "vmid", r.vmid, "generation", r.generation, "claim", r.id, "upid", view.task,
							"pid", view.pid, "uptime_seconds", view.uptime, "host_sample", view.hostAt, "memory_bytes", r.shape.MemoryBytes)
					}
				} else if !r.pacedProof.slotSettled {
					if t.completed.IsZero() {
						r.paced, r.inherited = false, true
					} else {
						// Keep checking a completed launch even after a slot-only
						// fallback, so a later authoritative observation can retire it.
						r.pacedProof.slotSettled = true
					}
					a.log.Warn("nodecap: quiet exact incomplete start no longer occupies a boot slot; retaining memory",
						"node", r.node, "vmid", r.vmid, "generation", r.generation, "claim", r.id, "task", view.task, "memory_bytes", r.shape.MemoryBytes)
				}
			}
		}
		a.mu.Unlock()
	}
}

func (a *Accountant) observeInactiveStart(ctx context.Context, t inactiveStartTarget, owner string) (inactiveStartView, bool) {
	base := "/nodes/" + url.PathEscape(t.node)
	user, _, _ := strings.Cut(owner, "!")
	const limit = 101
	list := func(query string, complete bool) ([]inactiveTask, bool) {
		var tasks []inactiveTask
		path := base + "/tasks?limit=101&userfilter=" + url.QueryEscape(user) + query
		err := a.cli.Get(ctx, path, &tasks)
		return tasks, err == nil && tasks != nil && (!complete || len(tasks) < limit)
	}
	owned := func(task inactiveTask) (bool, bool) {
		parts := strings.Split(task.UPID, ":")
		if len(parts) != 9 || parts[0] != "UPID" || parts[1] != t.node || task.Node != t.node || parts[5] != task.Kind || parts[6] != task.ID || parts[7] == "" {
			return false, false
		}
		start, err := strconv.ParseInt(parts[4], 16, 64)
		return parts[7] == owner, err == nil && start > 0 && start == task.Start
	}
	matches := func(task inactiveTask) bool {
		return task.ID == strconv.Itoa(t.vmid) && (t.kind == "qemu" && task.Kind == "qmstart" || t.kind == "lxc" && (task.Kind == "vzstart" || task.Kind == "vzreboot"))
	}
	idle := func() bool {
		tasks, ok := list("&source=active", true)
		if !ok {
			return false
		}
		for _, task := range tasks {
			isOwner, valid := owned(task)
			if !valid {
				return false
			}
			// A clone identifies its source, not its destination.
			if isOwner && (task.ID == strconv.Itoa(t.vmid) || task.Kind == "qmclone" || task.Kind == "vzclone") {
				return false
			}
		}
		return true
	}
	confirmed := !t.completed.IsZero()
	if !confirmed && !idle() {
		return inactiveStartView{}, false
	}
	upid := t.upid
	if parts := strings.Split(upid, ":"); upid != "" {
		if len(parts) != 9 || parts[0] != "UPID" {
			// A malformed receipt proves no identity. Recover it like a
			// missing response, not by asking PVE for a guessed path.
			upid = ""
		} else if parts[1] != t.node || parts[7] != owner || !matches(inactiveTask{Kind: parts[5], ID: parts[6]}) {
			// An explicit disagreement is not the same as no receipt.
			return inactiveStartView{}, false
		}
	}
	if confirmed && upid == "" {
		return inactiveStartView{}, false
	}
	if upid == "" {
		// A lost response may be recovered only from one unambiguous
		// exact-owner, exact-guest start archived in the request window.
		since := t.attempt.Add(-5 * time.Second).Unix()
		until := t.returned.Add(5 * time.Second).Unix()
		tasks, ok := list(fmt.Sprintf("&source=archive&vmid=%d&since=%d&until=%d", t.vmid, since, until), true)
		if !ok {
			return inactiveStartView{}, false
		}
		for _, task := range tasks {
			isOwner, valid := owned(task)
			if !valid {
				return inactiveStartView{}, false
			}
			if !isOwner || !matches(task) {
				continue
			}
			if upid != "" || task.Start < since || task.Start > until || task.End < task.Start || task.Status == "" {
				return inactiveStartView{}, false
			}
			upid = task.UPID
		}
	}
	anchor := upid == ""
	if anchor {
		// No task ever appeared for the request (AlreadyRunning, or a
		// response lost before any task existed). Establish that this
		// principal can see its own task archive via a positive older
		// receipt. An empty own-active response alone is not proof.
		cutoff := t.attempt.Add(-5 * time.Second).Unix()
		tasks, ok := list(fmt.Sprintf("&source=archive&until=%d", cutoff), false)
		if !ok {
			return inactiveStartView{}, false
		}
		for _, previous := range tasks {
			isOwner, valid := owned(previous)
			if !valid {
				return inactiveStartView{}, false
			}
			if isOwner && previous.Start < cutoff && previous.End >= previous.Start && previous.End <= cutoff && previous.Status == "OK" {
				upid = previous.UPID
				break
			}
		}
	}
	if upid == "" {
		return inactiveStartView{}, false
	}
	var task inactiveTask
	if err := a.cli.Get(ctx, base+"/tasks/"+url.PathEscape(upid)+"/status", &task); err != nil ||
		task.UPID != upid || task.Status != "stopped" || task.Exit == "" {
		return inactiveStartView{}, false
	}
	if isOwner, valid := owned(task); !isOwner || !valid {
		return inactiveStartView{}, false
	}
	if anchor {
		if task.Exit != "OK" || task.Start >= t.attempt.Add(-5*time.Second).Unix() {
			return inactiveStartView{}, false
		}
	} else if !matches(task) || task.Start < t.attempt.Add(-5*time.Second).Unix() || task.Start > t.returned.Add(5*time.Second).Unix() {
		return inactiveStartView{}, false
	}
	var guest struct {
		ID     json.RawMessage `json:"vmid"`
		Name   string          `json:"name"`
		Status string          `json:"status"`
		Lock   string          `json:"lock"`
		PID    int64           `json:"pid"`
		Uptime int64           `json:"uptime"`
		HA     struct {
			Managed *int `json:"managed"`
		} `json:"ha"`
	}
	path := fmt.Sprintf("%s/%s/%d/status/current", base, t.kind, t.vmid)
	guestAt := a.now()
	id := strconv.Itoa(t.vmid)
	if err := a.cli.Get(ctx, path, &guest); err != nil || (string(guest.ID) != id && string(guest.ID) != strconv.Quote(id)) || guest.Name != t.name || guest.HA.Managed == nil || *guest.HA.Managed != 0 ||
		guest.Lock != "" || !(guest.Status == "stopped" && guest.PID == 0 || guest.Status == "running" && guest.PID > 0 && guest.Uptime > 0) {
		return inactiveStartView{}, false
	}
	if confirmed {
		// This is direct process uptime, not pvestatd's lagged cluster
		// inventory. Tolerate only integral-second rounding and GET latency.
		bootAt := guestAt.Add(-time.Duration(guest.Uptime) * time.Second)
		if task.Exit != "OK" || guest.Status != "running" || bootAt.Before(t.attempt.Add(-2*time.Second)) ||
			bootAt.After(t.completed.Add(2*time.Second)) {
			return inactiveStartView{}, false
		}
	}
	host, err := a.nodeLiveStatus(ctx, t.node)
	hostAt := a.now()
	_, hasMemory := host.Memory.AvailableBytes()
	if err != nil || !hasMemory || !hostAt.After(t.attempt) || (confirmed && !hostAt.After(t.completed)) || (!confirmed && !idle()) {
		return inactiveStartView{}, false
	}
	return inactiveStartView{at: guestAt, hostAt: hostAt, status: guest.Status, pid: guest.PID, uptime: guest.Uptime, task: upid, host: host}, true
}
