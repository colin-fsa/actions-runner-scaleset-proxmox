package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/nodecap"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/pool"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/tags"
	"golang.org/x/sync/errgroup"
)

type recoveryReader interface {
	Get(context.Context, string, any) error
}

type restartTask struct {
	UPID   string `json:"upid"`
	Node   string `json:"node"`
	Type   string `json:"type"`
	ID     string `json:"id"`
	Start  int64  `json:"starttime"`
	End    int64  `json:"endtime"`
	Status string `json:"status"`
}

type restartGuest struct {
	ID        int     `json:"vmid"`
	Name      string  `json:"name"`
	Tags      string  `json:"tags"`
	Status    string  `json:"status"`
	Lock      string  `json:"lock"`
	Template  int     `json:"template"`
	Uptime    int64   `json:"uptime"`
	PID       int64   `json:"pid"`
	MaxMemory uint64  `json:"maxmem"`
	CPUs      float64 `json:"cpus"`
	kind      string
	set       string
	profile   string
	pretag    string
	untagged  bool
}

type restartView struct {
	uptime int64
	tasks  map[string]restartTask
	guests map[int]restartGuest
}

func scopedRestartConfig(cfg *config.Config) bool {
	for _, set := range cfg.Scalesets {
		if cfg.RecycleModeFor(set) != config.RecycleModeDestroy {
			return false
		}
	}
	return cfg.Cluster.Mode == "standalone" && cfg.Nodes.Strategy == "single" && cfg.Nodes.SingleNode != "" &&
		strings.Contains(cfg.Proxmox.Auth.TokenID, "!") && cfg.Pool.Capacity.Enabled &&
		cfg.Pool.Capacity.MemoryMode == config.MemoryModeHostAvailable &&
		max(cfg.Pool.BootsPerNode, cfg.Pool.WindowsBootsPerNode) > 0 && cfg.Pool.BootStartInterval.D() > 0 &&
		!cfg.Pool.Capacity.EvictIdleForDemand
}

func restartTaskOwner(t restartTask, node string) (string, error) {
	p := strings.Split(t.UPID, ":")
	if len(p) != 9 || p[0] != "UPID" || p[1] != node || t.Node != node || p[5] != t.Type ||
		p[6] != t.ID || p[7] == "" || t.Start <= 0 {
		return "", errors.New("malformed Proxmox task identity")
	}
	start, err := strconv.ParseInt(p[4], 16, 64)
	if err != nil || start != t.Start {
		return "", errors.New("malformed Proxmox task start")
	}
	return p[7], nil
}

func readRestartTasks(ctx context.Context, cli recoveryReader, node, token string, cutoff int64) (map[string]restartTask, error) {
	const limit = 201
	// PVE strips the token suffix from the JSON "user" before applying
	// userfilter. Search by parent user, then enforce exact owner in UPID.
	user, _, _ := strings.Cut(token, "!")
	base := "/nodes/" + url.PathEscape(node) + "/tasks?limit=" + strconv.Itoa(limit) + "&userfilter=" + url.QueryEscape(user)
	read := func(query string, requireFull bool) ([]restartTask, error) {
		var rows []restartTask
		if err := cli.Get(ctx, base+query, &rows); err != nil || rows == nil || (requireFull && len(rows) >= limit) {
			return nil, errors.New("Proxmox controller task view unavailable, incomplete or capped")
		}
		return rows, nil
	}
	exactOwner := func(t restartTask) (bool, error) {
		owner, err := restartTaskOwner(t, node)
		return owner == token, err
	}
	active, err := read("&source=active", true)
	if err != nil {
		return nil, err
	}
	for _, t := range active {
		own, err := exactOwner(t)
		if err != nil {
			return nil, err
		}
		if own {
			// Even finished but not yet archived entries, as well as
			// clones whose task ID names the template, postpone proof.
			return nil, fmt.Errorf("controller task %s is active or not durably archived", t.Type)
		}
	}
	tail, err := read("&source=archive&until="+strconv.FormatInt(cutoff, 10), false)
	if err != nil {
		return nil, err
	}
	var anchor restartTask
	for _, t := range tail {
		own, err := exactOwner(t)
		if err != nil {
			return nil, err
		}
		if own && t.End >= t.Start && t.End <= cutoff && t.Status == "OK" {
			anchor = t
			break
		}
	}
	if anchor.UPID == "" {
		return nil, errors.New("no prior successful exact-controller task at archive boundary (first install, token change, or missing history)")
	}
	var terminal struct {
		UPID, Node, Type, ID, Status, ExitStatus string
		Start                                    int64 `json:"starttime"`
	}
	taskPath := "/nodes/" + url.PathEscape(node) + "/tasks/" + url.PathEscape(anchor.UPID) + "/status"
	if err := cli.Get(ctx, taskPath, &terminal); err != nil ||
		terminal.UPID != anchor.UPID || terminal.Node != node || terminal.Type != anchor.Type || terminal.ID != anchor.ID ||
		terminal.Start != anchor.Start || terminal.Status != "stopped" || terminal.ExitStatus != "OK" {
		return nil, errors.New("could not confirm ordinary API visibility of an exact-controller archived task")
	}
	rows, err := read("&source=archive&since="+strconv.FormatInt(cutoff, 10), true)
	if err != nil {
		return nil, err
	}
	out := make(map[string]restartTask)
	for _, t := range rows {
		own, err := exactOwner(t)
		if err != nil {
			return nil, err
		}
		if !own {
			continue
		}
		if t.Start < cutoff || t.End < t.Start || t.Status == "" {
			return nil, errors.New("incomplete exact-controller archive task")
		}
		if _, duplicate := out[t.UPID]; duplicate {
			return nil, errors.New("duplicate exact-controller archive task")
		}
		out[t.UPID] = t
	}
	return out, nil
}

func readRestartView(ctx context.Context, cli recoveryReader, cfg *config.Config, cutoff int64) (restartView, error) {
	node := cfg.Nodes.SingleNode
	base := "/nodes/" + url.PathEscape(node)
	var nodes []struct{ Node, Status string }
	if err := cli.Get(ctx, "/nodes", &nodes); err != nil || len(nodes) != 1 || nodes[0].Node != node || nodes[0].Status != "online" {
		return restartView{}, errors.New("could not establish the sole online node")
	}
	var clock struct{ Time int64 }
	if err := cli.Get(ctx, base+"/time", &clock); err != nil || time.Since(time.Unix(clock.Time, 0)).Abs() > 5*time.Second {
		return restartView{}, errors.New("Proxmox task clock could not be verified")
	}
	firstTasks, err := readRestartTasks(ctx, cli, node, cfg.Proxmox.Auth.TokenID, cutoff)
	if err != nil {
		return restartView{}, err
	}
	view := restartView{guests: make(map[int]restartGuest)}
	for _, kind := range []string{"qemu", "lxc"} {
		var guests []restartGuest
		if err := cli.Get(ctx, base+"/"+kind, &guests); err != nil || guests == nil {
			return restartView{}, fmt.Errorf("unavailable %s inventory", kind)
		}
		for _, g := range guests {
			if g.ID <= 0 {
				return restartView{}, errors.New("invalid Proxmox guest identity")
			}
			if g.Template != 0 {
				continue
			}
			for _, set := range cfg.Scalesets {
				rng := entryVMIDRange(set, cfg.Proxmox.VMIDRange)
				inRange := g.ID >= rng.Min && g.ID <= rng.Max
				owned := tags.IsOwnedBy(g.Tags, set.Name)
				if !inRange && !owned {
					continue
				}
				if !inRange || g.Name != fmt.Sprintf("gh-runner-%s-%d", set.Name, g.ID) ||
					g.Lock != "" || (g.Status != "running" && g.Status != "stopped") || (g.Status == "running" && g.Uptime <= 0) {
					return restartView{}, fmt.Errorf("guest %d has ambiguous managed identity, power or lock", g.ID)
				}
				profile := tags.ProfileOf(g.Tags)
				if !owned {
					if g.Status != "stopped" || hasManagedGuestTags(g.Tags) {
						return restartView{}, fmt.Errorf("guest %d lacks trusted owner tags and is not a clean stopped pre-tag candidate", g.ID)
					}
					// Optional attribution must not outlive PVE task
					// retention or prevent excluding an inert occupied ID.
					_, g.pretag, _ = pretagProfile(ctx, cli, cfg, set, kind, g.ID, firstTasks)
					g.set, g.profile, g.kind, g.untagged = set.Name, tags.DefaultProfile, kind, true
				} else {
					for _, p := range set.Profiles {
						if p.Name == profile && p.GuestTypeOrDefault() == kind {
							g.set, g.profile, g.kind = set.Name, profile, kind
						}
					}
				}
				if g.set == "" || view.guests[g.ID].ID != 0 {
					return restartView{}, fmt.Errorf("guest %d has ambiguous profile or duplicate identity", g.ID)
				}
				view.guests[g.ID] = g
			}
		}
	}
	// /status/current verifies running PID/uptime and managed HA
	// via the same guest-level VM.Audit used by ordinary adoption.
	type statusObservation struct {
		restartGuest
		HA struct {
			Managed *int `json:"managed"`
		} `json:"ha"`
	}
	type observedGuest struct {
		id   int
		live restartGuest
	}
	observed := make(chan observedGuest, len(view.guests))
	group, statusCtx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	for id, listed := range view.guests {
		group.Go(func() error {
			var snapshot statusObservation
			path := fmt.Sprintf("%s/%s/%d/status/current", base, listed.kind, id)
			if err := cli.Get(statusCtx, path, &snapshot); err != nil {
				return fmt.Errorf("guest %d direct status unavailable", id)
			}
			live := snapshot.restartGuest
			if snapshot.HA.Managed == nil || *snapshot.HA.Managed != 0 || live.ID != id ||
				live.Name != listed.Name || live.Tags != listed.Tags || live.Status != listed.Status ||
				live.Lock != "" || live.Template != 0 ||
				(live.Status == "running" && (live.PID <= 0 || live.Uptime <= 0)) ||
				(listed.untagged && (live.PID != 0 || live.Uptime != 0)) {
				return fmt.Errorf("guest %d HA, process or power is ambiguous", id)
			}
			live.kind, live.set, live.profile, live.pretag, live.untagged = listed.kind, listed.set, listed.profile, listed.pretag, listed.untagged
			observed <- observedGuest{id, live}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return restartView{}, err
	}
	close(observed)
	for g := range observed {
		view.guests[g.id] = g.live
	}
	var host struct {
		Uptime int64              `json:"uptime"`
		Memory nodecap.HostMemory `json:"memory"`
	}
	hostErr := cli.Get(ctx, base+"/status", &host)
	_, hasAvailable := host.Memory.AvailableBytes()
	if hostErr != nil || host.Uptime <= 0 || !hasAvailable {
		return restartView{}, errors.New("authoritative host available memory unavailable")
	}
	view.uptime = host.Uptime
	view.tasks, err = readRestartTasks(ctx, cli, node, cfg.Proxmox.Auth.TokenID, cutoff)
	if err != nil {
		return restartView{}, err
	}
	if !sameRestartTasks(firstTasks, view.tasks) {
		return restartView{}, errors.New("controller tasks changed during inventory and node read")
	}
	return view, nil
}

func sameRestartTasks(a, b map[string]restartTask) bool {
	if len(a) != len(b) {
		return false
	}
	for id, task := range a {
		if b[id] != task {
			return false
		}
	}
	return true
}

type restartCleanup struct {
	manager string
	row     pool.RowSnapshot
}

type restartLateGuest struct {
	manager string
	vm      provisioner.VM
	pretag  bool
	receipt string
}
type restartResult struct {
	holds   []nodecap.RestartHold
	stopped []restartCleanup
	late    []restartLateGuest
}

// The old controller must be fenced by standalone deployment before
// this proof. Only its unchanged, dedicated PVE token is in scope.
func proveScopedRestart(ctx context.Context, cli recoveryReader, cfg *config.Config, adopted map[string][]pool.RowSnapshot, ready func(context.Context, *provisioner.VM) error, cutoff int64, quiet time.Duration, result *restartResult) error {
	first, err := readRestartView(ctx, cli, cfg, cutoff)
	if err != nil {
		return err
	}
	rows := make(map[int]pool.RowSnapshot)
	for set, snapshots := range adopted {
		for _, row := range snapshots {
			g, found := first.guests[row.VMID]
			if !found || rows[row.VMID].VMID != 0 || g.set != set || g.nameMismatch(row, cfg.Nodes.SingleNode) {
				return fmt.Errorf("adopted guest %d does not match exact Proxmox inventory", row.VMID)
			}
			if g.untagged && (!row.RestartPretagQuarantine || !row.AdoptionProtected || row.State != "excluded" || row.RunnerID != 0 || row.JITPending) {
				return fmt.Errorf("pre-tag guest %d is not protected from assignment", row.VMID)
			}
			rows[row.VMID] = row
		}
	}
	if len(rows) != len(first.guests) {
		for id, g := range first.guests {
			if _, adopted := rows[id]; !adopted && g.untagged {
				if _, err := secondScopedRestartView(ctx, cli, cfg, cutoff, quiet, first); err != nil {
					return err
				}
				break
			}
		}
		for id, g := range first.guests {
			if _, adopted := rows[id]; !adopted {
				result.late = append(result.late, restartLateGuest{manager: g.set, pretag: g.untagged, receipt: g.pretag,
					vm: provisioner.VM{VMID: id, Node: cfg.Nodes.SingleNode, Name: g.Name, Profile: g.profile, GuestType: g.kind}})
			}
		}
		// This bracketed observation is enough to insert a row, never
		// enough to open admission. The complete proof will run again.
		return errors.New("managed Proxmox inventory not represented by adoption")
	}
	var unready sync.Map
	probes, probeCtx := errgroup.WithContext(ctx)
	probes.SetLimit(4)
	for id, g := range first.guests {
		if g.Status != "running" {
			continue
		}
		row := rows[id]
		probes.Go(func() error {
			claimed := (row.State == "running" || row.State == "assigned") && row.RunnerID > 0
			if row.State != "hot" && !claimed ||
				ready(probeCtx, &provisioner.VM{VMID: id, Name: g.Name, Node: cfg.Nodes.SingleNode, Profile: g.profile, GuestType: g.kind, CreatedAt: row.CreatedAt}) != nil {
				if g.MaxMemory == 0 || g.CPUs <= 0 || g.CPUs > 4096 {
					return fmt.Errorf("guest %d readiness and configured footprint are unproven", id)
				}
				unready.Store(id, nodecap.RestartHold{VMID: id, Node: cfg.Nodes.SingleNode, Generation: row.CreatedAt,
					Shape: nodecap.Shape{MemoryBytes: g.MaxMemory, VCPUs: int(math.Ceil(g.CPUs))}})
			}
			return nil
		})
	}
	if err := probes.Wait(); err != nil {
		return err
	}
	second, err := secondScopedRestartView(ctx, cli, cfg, cutoff, quiet, first)
	if err != nil {
		return err
	}
	for id, g := range second.guests {
		if g.Status == "stopped" && rows[id].RestartCleanupPending {
			result.stopped = append(result.stopped, restartCleanup{manager: g.set, row: rows[id]})
		}
	}
	unready.Range(func(_, value any) bool {
		result.holds = append(result.holds, value.(nodecap.RestartHold))
		return true
	})
	return nil
}

func secondScopedRestartView(ctx context.Context, cli recoveryReader, cfg *config.Config, cutoff int64, quiet time.Duration, first restartView) (restartView, error) {
	timer := time.NewTimer(quiet)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return restartView{}, ctx.Err()
	case <-timer.C:
	}
	second, err := readRestartView(ctx, cli, cfg, cutoff)
	if err != nil {
		return restartView{}, err
	}
	if second.uptime <= first.uptime || !sameRestartTasks(first.tasks, second.tasks) || len(second.guests) != len(first.guests) {
		return restartView{}, errors.New("node, controller tasks, or managed inventory changed across recovery reads")
	}
	for id, a := range first.guests {
		b := second.guests[id]
		if a.Name != b.Name || a.Tags != b.Tags || a.Status != b.Status || a.untagged != b.untagged || a.kind != b.kind || a.MaxMemory != b.MaxMemory || a.CPUs != b.CPUs ||
			(a.Status == "running" && (b.PID != a.PID || b.Uptime <= a.Uptime || absSeconds((second.uptime-first.uptime)-(b.Uptime-a.Uptime)) > 5)) {
			return restartView{}, fmt.Errorf("guest %d changed boot identity across recovery reads", id)
		}
	}
	return second, nil
}

func (g restartGuest) nameMismatch(row pool.RowSnapshot, node string) bool {
	return row.Node != node || row.Name != g.Name || row.Profile != g.profile || row.CreatedAt.IsZero()
}

func absSeconds(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

type restartBarrier struct {
	mu       sync.Mutex
	managers map[string]pool.Manager
	revision uint64
	ready    chan struct{}
}

func (b *restartBarrier) released() bool {
	select {
	case <-b.ready:
		return true
	default:
		return false
	}
}

func (b *restartBarrier) track(set string, mgr pool.Manager) func() {
	b.mu.Lock()
	b.managers[set] = mgr
	b.revision++
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.managers[set] == mgr {
			delete(b.managers, set)
			b.revision++
		}
	}
}

func (b *restartBarrier) snapshot() (map[string]pool.Manager, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	snapshot := make(map[string]pool.Manager, len(b.managers))
	for set, manager := range b.managers {
		snapshot[set] = manager
	}
	return snapshot, b.revision
}

func (b *restartBarrier) releaseCurrent(revision uint64, release func() error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.revision != revision {
		return errors.New("pool manager changed during restart observation")
	}
	if err := release(); err != nil {
		return err
	}
	close(b.ready)
	return nil
}

func (b *restartBarrier) observeLate(ctx context.Context, revision uint64, mgrs map[string]pool.Manager, guests []restartLateGuest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.revision != revision || b.released() {
		return errors.New("pool manager changed during late guest observation")
	}
	for _, guest := range guests {
		mgr, ok := mgrs[guest.manager].(interface {
			AdoptLateRestartGuest(context.Context, *provisioner.VM) error
		})
		if !ok {
			return errors.New("exact late guest observation unavailable")
		}
		if guest.pretag {
			protected, ok := mgrs[guest.manager].(interface {
				QuarantinePretagRestartGuest(context.Context, *provisioner.VM, string) error
			})
			if !ok {
				return errors.New("pre-tag quarantine unavailable")
			}
			if err := protected.QuarantinePretagRestartGuest(ctx, &guest.vm, guest.receipt); err != nil {
				return err
			}
			continue
		}
		if err := mgr.AdoptLateRestartGuest(ctx, &guest.vm); err != nil {
			return err
		}
	}
	return nil
}

func completeLeaderRestart(ctx context.Context, log *slog.Logger, cfg *config.Config, prov provisioner.Provisioner, b *restartBarrier, release func([]nodecap.RestartHold, func()) error) error {
	if !scopedRestartConfig(cfg) {
		log.Error("controller recovery blocked: exact-owner restart requires paced standalone single-node host_available, destroy mode, and token auth")
		<-ctx.Done()
		return nil
	}
	cutoff := time.Now().Add(-max(cfg.Pool.CloneInflightGrace.D(), cfg.Pool.BootStartInterval.D()) - 5*time.Second).Unix()
	quiet := max(cfg.Pool.BootStartInterval.D(), 15*time.Second)
	var lastReason string
	for ctx.Err() == nil {
		mgrs, revision := b.snapshot()
		rows := make(map[string][]pool.RowSnapshot, len(mgrs))
		var err error
		attempt, cancel := context.WithTimeout(ctx, quiet+45*time.Second)
		if len(mgrs) != len(cfg.Scalesets) {
			err = errors.New("waiting for all managed pools to observe inherited rows")
		}
		if err == nil {
			for set, m := range mgrs {
				rows[set], err = m.ListRows(attempt)
				if err != nil {
					break
				}
			}
		}
		var result restartResult
		if err == nil {
			err = proveScopedRestart(attempt, prov.Client(), cfg, rows, func(ctx context.Context, vm *provisioner.VM) error {
				ping, cancel := context.WithTimeout(ctx, 8*time.Second)
				defer cancel()
				return prov.WaitReady(ping, vm, 8*time.Second)
			}, cutoff, quiet, &result)
		}
		if len(result.late) > 0 && attempt.Err() == nil {
			if observeErr := b.observeLate(attempt, revision, mgrs, result.late); observeErr != nil {
				err = fmt.Errorf("late exact guest adoption: %w", observeErr)
			}
		}
		if err == nil && attempt.Err() == nil {
			err = b.releaseCurrent(revision, func() error {
				byID := map[int]pool.Manager{}
				for set, snapshots := range rows {
					for _, row := range snapshots {
						byID[row.VMID] = mgrs[set]
					}
				}
				valid := result.holds[:0]
				for _, hold := range result.holds {
					guard, ok := byID[hold.VMID].(interface {
						QuarantineRestartHold(int, string, time.Time) (bool, error)
					})
					if !ok {
						return errors.New("inherited guest quarantine unavailable")
					}
					retain, err := guard.QuarantineRestartHold(hold.VMID, hold.Node, hold.Generation)
					if err != nil {
						return err
					}
					if retain {
						valid = append(valid, hold)
					}
				}
				for _, stopped := range result.stopped {
					if cleaner, ok := mgrs[stopped.manager].(interface {
						QueueObservedRestartCleanup(context.Context, pool.RowSnapshot) error
					}); ok {
						observe, stop := context.WithTimeout(ctx, 8*time.Second)
						if err := cleaner.QueueObservedRestartCleanup(observe, stopped.row); err != nil {
							log.Warn("controller restart: stopped cleanup deferred", "vmid", stopped.row.VMID, "reason", err)
						}
						stop()
					}
				}
				return release(valid, func() {
					for _, h := range valid {
						m := byID[h.VMID]
						snapshots, err := m.ListRows(attempt)
						if err != nil {
							continue
						}
						found := false
						for _, row := range snapshots {
							if row.VMID == h.VMID && row.Node == h.Node && row.CreatedAt.Equal(h.Generation) {
								found = true
								break
							}
						}
						if !found {
							// No creates can race while the gate is
							// closed. Exact row removal is confirmed.
							if cleaner, ok := m.(interface {
								ForgetRestartAccounting(int, string, time.Time)
							}); ok {
								cleaner.ForgetRestartAccounting(h.VMID, h.Node, h.Generation)
							}
						}
					}
				})
			})
		} else if err == nil {
			err = attempt.Err()
		}
		cancel()
		if err == nil && ctx.Err() == nil {
			log.Info("controller recovery: verified exact controller tasks and adopted guest state", "inherited_debits", len(result.holds))
			return nil
		}
		if err != nil && err.Error() != lastReason && ctx.Err() == nil {
			lastReason = err.Error()
			log.Warn("controller recovery blocked; rechecking observed tasks and inventory", "reason", lastReason)
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return nil
}
