package provisioner

// LXC (Proxmox container) support.
//
// A profile with guest_type: lxc clones an unprivileged container
// template instead of a QEMU VM. Every Provisioner method dispatches on
// the guest type (see resolveGuestType) and the container variants live
// here. They use the go-proxmox client's raw Get/Post/Put/Delete helpers
// rather than its Container type: the typed wrapper drops full=0 on
// linked clones (IntOrBool+omitempty), expects a task UPID from the
// synchronous PUT /config, and does not model the `env` option.
//
// JIT delivery: containers have no guest agent. InjectJITConfig stores
// ACTIONS_RUNNER_JIT_CONFIG in the container's `env` option (PVE 9.x;
// rendered as lxc.environment.runtime) and restarts the container so its
// init process receives it. The in-guest bootstrap
// (scripts/linux/lxc-runner-bootstrap.sh) reads it from /proc/1/environ,
// runs the runner once and powers the container off, which the pool's
// power poller observes as job completion — the same contract the QEMU
// Linux runner unit follows.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/luthermonson/go-proxmox"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/nodecap"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/tags"
)

// ErrLXCSnapshotUnsupported is returned by SnapshotCreate /
// SnapshotRollback for containers. Config validation rejects
// guest_type: lxc with recycle_mode snapshot_rollback, so reaching this
// means a caller bypassed validation; the pool's rollback-failure path
// falls back to destroy.
var ErrLXCSnapshotUnsupported = errors.New("provisioner: snapshot recycling is not supported for lxc containers")

// lxcJITEnvKey is the environment variable the in-guest bootstrap reads
// from /proc/1/environ.
const lxcJITEnvKey = "ACTIONS_RUNNER_JIT_CONFIG"

// lxcJITRestartTTL bounds a jitRestarts entry if a restart hangs and the
// deferred delete never runs.
const lxcJITRestartTTL = 10 * time.Minute

// lxcRebootTimeoutSeconds is the graceful-shutdown budget passed to
// /status/reboot before PVE kills the container. An idle hot container
// has nothing to flush, so this is short.
const lxcRebootTimeoutSeconds = 30

// lxcShutdownTimeoutSeconds mirrors the QEMU stop path's 60s graceful
// window; forceStop makes PVE hard-stop the container afterwards.
const lxcShutdownTimeoutSeconds = 60

// lxcReadyPollInterval is how often waitReadyLXC polls status/current.
// Variable so tests can compress it.
var lxcReadyPollInterval = time.Second

// lxcPostRestartReadyTimeout bounds how long InjectJITConfig waits for
// the restarted container to report running.
var lxcPostRestartReadyTimeout = 60 * time.Second

// lxcListEntry is one row of GET /nodes/{node}/lxc.
type lxcListEntry struct {
	VMID   proxmox.StringOrUint64 `json:"vmid"`
	Name   string                 `json:"name"`
	Tags   string                 `json:"tags"`
	Status string                 `json:"status"`
}

// lxcStatus is the subset of GET /nodes/{node}/lxc/{vmid}/status/current
// the provisioner reads.
type lxcStatus struct {
	Status string `json:"status"`
	Name   string `json:"name"`
}

func lxcPath(node string, vmid int, suffix string) string {
	return fmt.Sprintf("/nodes/%s/lxc/%d%s", node, vmid, suffix)
}

// ---------------------------------------------------------------------------
// Guest-type resolution
// ---------------------------------------------------------------------------

// hasLXCProfile reports whether any profile of this scale set clones
// containers. When none does, resolveGuestType never probes: QEMU-only
// deployments make exactly the API calls they made before LXC support.
func (p *pmox) hasLXCProfile() bool {
	for _, prof := range p.profiles {
		if prof.GuestTypeOrDefault() == config.GuestTypeLXC {
			return true
		}
	}
	return false
}

// profileGuestType returns the configured guest type of a named profile.
// The empty name maps to the default profile, as elsewhere.
func (p *pmox) profileGuestType(name string) (string, bool) {
	if name == "" {
		name = tags.DefaultProfile
	}
	prof, ok := p.profiles[name]
	if !ok {
		return "", false
	}
	return prof.GuestTypeOrDefault(), true
}

// templateGuestType reports whether a template VMID belongs to an LXC
// profile (as its template or canary template). Config validation
// requires LXC profiles to name their template explicitly, so the global
// proxmox.template_vmid is always treated as a QEMU template.
func (p *pmox) templateGuestType(vmid int) string {
	for _, prof := range p.profiles {
		if prof.GuestTypeOrDefault() != config.GuestTypeLXC {
			continue
		}
		if prof.TemplateVMID == vmid || (prof.CanaryTemplateVMID > 0 && prof.CanaryTemplateVMID == vmid) {
			return config.GuestTypeLXC
		}
	}
	return config.GuestTypeQEMU
}

// cloneGuestType picks the guest type for a clone: the explicit option
// first, then the profile's configured type.
func (p *pmox) cloneGuestType(opts CloneOptions) string {
	if opts.GuestType != "" {
		return opts.GuestType
	}
	if t, ok := p.profileGuestType(opts.Profile); ok {
		return t
	}
	return config.GuestTypeQEMU
}

// ErrGuestTypeUnknown is returned when a guest's type (qemu or lxc) has
// to be probed and the probe fails for a reason other than "no such
// container". Callers must treat it as retryable: assuming qemu here
// would let Destroy report a container as already gone (the qemu lookup
// says "does not exist") and leak it.
var ErrGuestTypeUnknown = errors.New("provisioner: cannot determine guest type")

// resolveGuestType decides whether vm is a QEMU VM or an LXC container:
//
//  1. vm.GuestType when set (Clone and ListOwnedVMs populate it);
//  2. qemu when no profile of this scale set is LXC (no probing, so
//     QEMU-only deployments are unaffected);
//  3. the type of vm.Profile when it names a known profile;
//  4. otherwise a probe of GET /lxc/{vmid}/status/current: success means
//     a container, "does not exist" means qemu (or absent from both — the
//     qemu path then reports it as already gone), and any other failure
//     returns ErrGuestTypeUnknown. Only reached for call sites that carry
//     no profile (e.g. the pool's destroy path).
//
// VMIDs are unique across QEMU and LXC cluster-wide, so the probe cannot
// confuse two guests.
func (p *pmox) resolveGuestType(ctx context.Context, vm *VM) (string, error) {
	if vm == nil {
		return config.GuestTypeQEMU, nil
	}
	if vm.GuestType != "" {
		return vm.GuestType, nil
	}
	if !p.hasLXCProfile() {
		return config.GuestTypeQEMU, nil
	}
	if vm.Profile != "" {
		if t, ok := p.profileGuestType(vm.Profile); ok {
			return t, nil
		}
	}
	_, err := p.lxcStatus(ctx, vm.Node, vm.VMID)
	switch {
	case err == nil:
		return config.GuestTypeLXC, nil
	case isNotFound(err):
		return config.GuestTypeQEMU, nil
	default:
		return "", fmt.Errorf("%w for vmid %d on %s: %w", ErrGuestTypeUnknown, vm.VMID, vm.Node, classifyProxmoxError(err))
	}
}

// ---------------------------------------------------------------------------
// Template discovery
// ---------------------------------------------------------------------------

// lxcTemplateOn reports whether node holds container vmid and whether
// it is a template.
func (p *pmox) lxcTemplateOn(ctx context.Context, node string, vmid int) (found, template bool) {
	var cfg map[string]any
	if err := p.cli.Get(ctx, lxcPath(node, vmid, "/config"), &cfg); err != nil {
		return false, false
	}
	return true, truthy(cfg["template"])
}

// discoverLXCTemplateNode is discoverTemplateNodeFor for a container
// template, with the same per-node timeout and error shape.
func (p *pmox) discoverLXCTemplateNode(ctx context.Context, statuses proxmox.NodeStatuses, vmid int) error {
	var nonTemplateHits []string
	for _, ns := range statuses {
		nodeCtx, cancel := context.WithTimeout(ctx, templateDiscoveryTimeoutPerNode)
		found, isTmpl := p.lxcTemplateOn(nodeCtx, ns.Node, vmid)
		cancel()
		if !found {
			continue
		}
		if !isTmpl {
			p.log.Warn("provisioner: container VMID found but not a template; continuing scan",
				"vmid", vmid, "node", ns.Node)
			nonTemplateHits = append(nonTemplateHits, ns.Node)
			continue
		}
		p.templateNode = ns.Node
		p.templateNodeVMID = vmid
		return nil
	}
	if len(nonTemplateHits) > 0 {
		return fmt.Errorf("%w: lxc vmid=%d found on %v but none are templates",
			ErrTemplateNotFound, vmid, nonTemplateHits)
	}
	return fmt.Errorf("%w: lxc vmid=%d", ErrTemplateNotFound, vmid)
}

// locateLXCTemplate is locateTemplate for a container template.
func (p *pmox) locateLXCTemplate(ctx context.Context, statuses proxmox.NodeStatuses, vmid int) (string, error) {
	for _, ns := range statuses {
		nodeCtx, cancel := context.WithTimeout(ctx, templateDiscoveryTimeoutPerNode)
		found, isTmpl := p.lxcTemplateOn(nodeCtx, ns.Node, vmid)
		cancel()
		if found && isTmpl {
			return ns.Node, nil
		}
	}
	return "", fmt.Errorf("%w: lxc vmid=%d", ErrTemplateNotFound, vmid)
}

// ---------------------------------------------------------------------------
// Clone
// ---------------------------------------------------------------------------

// buildLXCCloneParams renders the POST /lxc/{template}/clone body.
// Linked clones send full=0 explicitly and stay on the template node;
// full clones may target another node and storage, as for QEMU.
func buildLXCCloneParams(scaleSetName string, opts CloneOptions, templateNodeName string) map[string]any {
	params := map[string]any{
		"newid":       opts.NewVMID,
		"hostname":    opts.Name,
		"description": fmt.Sprintf("GitHub Actions runner managed by actions-runner-scaleset-proxmox (scale set %q, profile %q).", scaleSetName, profileOrDefault(opts.Profile)),
	}
	if opts.ProxmoxPool != "" {
		params["pool"] = opts.ProxmoxPool
	}
	if opts.Linked {
		params["full"] = 0
		return params
	}
	params["full"] = 1
	if opts.Node != "" && opts.Node != templateNodeName {
		params["target"] = opts.Node
	}
	if opts.Storage != "" {
		params["storage"] = opts.Storage
	}
	return params
}

func profileOrDefault(name string) string {
	if name == "" {
		return tags.DefaultProfile
	}
	return name
}

// buildLXCCloneConfig renders the PUT /lxc/{id}/config body applied to
// a fresh clone: the same owner/scale-set/profile/template tags a QEMU
// clone gets, protection off, the hardware shape, swap=0 (the memory
// limit is a hard cgroup limit; swap would hide pressure and slow jobs)
// and, with the firewall enabled, firewall=1 on every template NIC.
func buildLXCCloneConfig(scaleSetName string, opts CloneOptions, ctCfg map[string]any, firewallOn bool) (map[string]any, error) {
	initial, err := tags.Initial(scaleSetName, opts.Profile, opts.TemplateClass)
	if err != nil {
		return nil, fmt.Errorf("compute initial tags: %w", err)
	}
	body := map[string]any{
		"tags":       tags.Encode(initial),
		"protection": 0,
		"swap":       0,
	}
	if opts.CPUCores > 0 {
		body["cores"] = opts.CPUCores
	}
	if opts.MemoryMB > 0 {
		body["memory"] = opts.MemoryMB
	}
	if firewallOn {
		for k, v := range lxcNICFirewallPatches(ctCfg) {
			body[k] = v
		}
	}
	return body, nil
}

// lxcNICFirewallPatches returns net<N> values from a container config
// that need firewall=1 added or corrected. LXC NIC strings
// ("name=eth0,bridge=vmbr0,hwaddr=...,ip=dhcp,type=veth") use the same
// comma-separated key=value syntax, so forceNICFirewallFlag applies.
func lxcNICFirewallPatches(ctCfg map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range ctCfg {
		var idx int
		if _, err := fmt.Sscanf(k, "net%d", &idx); err != nil || fmt.Sprintf("net%d", idx) != k {
			continue
		}
		val, ok := v.(string)
		if !ok || val == "" {
			continue
		}
		if patched, changed := forceNICFirewallFlag(val); changed {
			out[k] = patched
		}
	}
	return out
}

// cloneLXC is Clone for a container template. Ordering matches the QEMU
// path: clone, tags + overrides, disk resize, firewall, then the
// optional start — the sandbox is in place before first boot.
func (p *pmox) cloneLXC(ctx context.Context, opts CloneOptions, templateVMID int, templateNodeName string) (*VM, error) {
	if err := p.cloneLXCFromTemplate(ctx, opts, templateVMID, templateNodeName); err != nil {
		return nil, err
	}

	resultNode := opts.Node
	if opts.Linked || resultNode == "" {
		resultNode = templateNodeName
	}
	vm := &VM{VMID: opts.NewVMID, Node: resultNode, Name: opts.Name, GuestType: config.GuestTypeLXC}

	var ctCfg map[string]any
	if err := p.cli.Get(ctx, lxcPath(resultNode, opts.NewVMID, "/config"), &ctCfg); err != nil {
		return nil, fmt.Errorf("fetch cloned container config: %w", err)
	}
	body, err := buildLXCCloneConfig(p.scaleSetName, opts, ctCfg, p.cfg.Firewall.Enabled)
	if err != nil {
		return nil, err
	}
	// PUT /lxc/{id}/config is synchronous; on a stopped container the
	// change is applied immediately rather than left pending.
	if err := p.cli.Put(ctx, lxcPath(resultNode, opts.NewVMID, "/config"), body, nil); err != nil {
		return nil, fmt.Errorf("set owner tags / overrides: %w", err)
	}

	if opts.DiskGB > 0 {
		var rupid proxmox.UPID
		if err := p.cli.Put(ctx, lxcPath(resultNode, opts.NewVMID, "/resize"),
			map[string]any{"disk": "rootfs", "size": fmt.Sprintf("%dG", opts.DiskGB)}, &rupid); err != nil {
			return nil, fmt.Errorf("resize rootfs: %w", err)
		}
		if rupid != "" {
			if err := awaitTask(ctx, proxmox.NewTask(rupid, p.cli), 120); err != nil {
				return nil, fmt.Errorf("await resize rootfs: %w", err)
			}
		}
	}

	if p.cfg.Firewall.Enabled {
		if err := p.applyFirewallLXC(ctx, vm); err != nil {
			return nil, fmt.Errorf("apply firewall: %w", err)
		}
	}

	if opts.PoweredOn {
		if err := p.startLXCTask(ctx, vm); err != nil {
			return nil, err
		}
	}
	return vm, nil
}

// lxcCloneMaxAttempts / lxcCloneRetryBase bound the retry of a container
// clone that fails because the source template is locked. Variables so
// tests can compress them.
var (
	lxcCloneMaxAttempts = 6
	lxcCloneRetryBase   = 2 * time.Second
)

// lxcTemplateLock returns the mutex serialising clones of one container
// template.
func (p *pmox) lxcTemplateLock(templateVMID int) *sync.Mutex {
	p.lxcCloneMu.Lock()
	defer p.lxcCloneMu.Unlock()
	if p.lxcCloneLocks == nil {
		p.lxcCloneLocks = map[int]*sync.Mutex{}
	}
	mu, ok := p.lxcCloneLocks[templateVMID]
	if !ok {
		mu = &sync.Mutex{}
		p.lxcCloneLocks[templateVMID] = mu
	}
	return mu
}

// isLockedError recognises PVE's "CT is locked (disk)" family.
func isLockedError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is locked")
}

// cloneLXCFromTemplate issues the clone and waits for its task.
//
// Unlike qemu, PVE holds `lock: disk` on the SOURCE container template
// for the whole clone, so a second concurrent clone of the same template
// dies with "CT is locked (disk)". Clones of one template are therefore
// serialised by a per-template mutex (held across issue + wait), and a
// lock error — e.g. from another orchestrator or a manual clone — is
// retried with bounded exponential backoff. Other errors fail at once.
func (p *pmox) cloneLXCFromTemplate(ctx context.Context, opts CloneOptions, templateVMID int, templateNodeName string) error {
	mu := p.lxcTemplateLock(templateVMID)
	mu.Lock()
	defer mu.Unlock()
	params := buildLXCCloneParams(p.scaleSetName, opts, templateNodeName)
	delay := lxcCloneRetryBase
	for attempt := 1; ; attempt++ {
		var task *proxmox.Task
		submit := func(ctx context.Context) error {
			var err error
			task, err = p.submitLXCTask(ctx, &VM{VMID: templateVMID, Node: templateNodeName}, "/clone", params)
			return err
		}
		var err error
		if opts.Submission != nil {
			err = opts.Submission(ctx, submit)
		} else {
			err = submit(ctx)
		}
		if err == nil {
			err = awaitTask(ctx, task, int(cloneTaskTimeout/time.Second))
		}
		if err == nil {
			return nil
		}
		if !isLockedError(err) || attempt >= lxcCloneMaxAttempts {
			return fmt.Errorf("lxc clone of template %d: %w", templateVMID, err)
		}
		p.log.Info("lxc clone: source template locked; retrying",
			"template_vmid", templateVMID, "new_vmid", opts.NewVMID, "attempt", attempt, "backoff", delay)
		select {
		case <-ctx.Done():
			return fmt.Errorf("lxc clone of template %d: %w", templateVMID, ctx.Err())
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// ---------------------------------------------------------------------------
// Firewall
// ---------------------------------------------------------------------------

func (p *pmox) lxcFirewallOptions() *proxmox.FirewallVirtualMachineOption {
	return &proxmox.FirewallVirtualMachineOption{
		Enable: true,
		Dhcp:   proxmox.IntOrBool(p.cfg.Firewall.DHCPOrDefault()),
	}
}

func (p *pmox) lxcGroupRule() *proxmox.FirewallRule {
	return &proxmox.FirewallRule{Type: "group", Action: p.cfg.Firewall.SecurityGroup, Enable: 1}
}

// applyFirewallLXC is applyFirewall for a container: the security-group
// rule first, then enable the per-guest firewall, both via
// /lxc/{id}/firewall. Failure fails the clone, as for QEMU.
func (p *pmox) applyFirewallLXC(ctx context.Context, vm *VM) error {
	if err := p.cli.Post(ctx, lxcPath(vm.Node, vm.VMID, "/firewall/rules"), p.lxcGroupRule(), nil); err != nil {
		return fmt.Errorf("attach security group %q to ct %d (node %s): %w",
			p.cfg.Firewall.SecurityGroup, vm.VMID, vm.Node, err)
	}
	if err := p.cli.Put(ctx, lxcPath(vm.Node, vm.VMID, "/firewall/options"), p.lxcFirewallOptions(), nil); err != nil {
		return fmt.Errorf("enable firewall on ct %d (node %s): %w", vm.VMID, vm.Node, err)
	}
	p.log.Info("firewall applied to clone",
		"vmid", vm.VMID, "node", vm.Node, "guest_type", config.GuestTypeLXC,
		"security_group", p.cfg.Firewall.SecurityGroup, "dhcp", p.cfg.Firewall.DHCPOrDefault())
	return nil
}

// ensureFirewallLXC is ensureFirewall for a container: add the group
// rule if missing, re-apply the options (idempotent PUT), and force
// firewall=1 onto every NIC.
func (p *pmox) ensureFirewallLXC(ctx context.Context, vm *VM) error {
	var rules []*proxmox.FirewallRule
	if err := p.cli.Get(ctx, lxcPath(vm.Node, vm.VMID, "/firewall/rules"), &rules); err != nil {
		return fmt.Errorf("list firewall rules on ct %d (node %s): %w", vm.VMID, vm.Node, err)
	}
	hasGroup := false
	for _, r := range rules {
		if r != nil && r.Type == "group" && r.Action == p.cfg.Firewall.SecurityGroup && r.Enable == 1 {
			hasGroup = true
			break
		}
	}
	if !hasGroup {
		if err := p.cli.Post(ctx, lxcPath(vm.Node, vm.VMID, "/firewall/rules"), p.lxcGroupRule(), nil); err != nil {
			return fmt.Errorf("attach security group %q to ct %d (node %s): %w",
				p.cfg.Firewall.SecurityGroup, vm.VMID, vm.Node, err)
		}
	}
	if err := p.cli.Put(ctx, lxcPath(vm.Node, vm.VMID, "/firewall/options"), p.lxcFirewallOptions(), nil); err != nil {
		return fmt.Errorf("enable firewall on ct %d (node %s): %w", vm.VMID, vm.Node, err)
	}
	var ctCfg map[string]any
	if err := p.cli.Get(ctx, lxcPath(vm.Node, vm.VMID, "/config"), &ctCfg); err != nil {
		return fmt.Errorf("read config of ct %d (node %s): %w", vm.VMID, vm.Node, err)
	}
	patches := lxcNICFirewallPatches(ctCfg)
	if len(patches) > 0 {
		body := make(map[string]any, len(patches))
		for k, v := range patches {
			body[k] = v
		}
		if err := p.cli.Put(ctx, lxcPath(vm.Node, vm.VMID, "/config"), body, nil); err != nil {
			return fmt.Errorf("force firewall=1 on NICs of ct %d (node %s): %w", vm.VMID, vm.Node, err)
		}
	}
	if !hasGroup || len(patches) > 0 {
		p.log.Info("firewall repaired before start",
			"vmid", vm.VMID, "node", vm.Node, "guest_type", config.GuestTypeLXC,
			"security_group", p.cfg.Firewall.SecurityGroup,
			"group_added", !hasGroup, "nics_patched", len(patches))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Power
// ---------------------------------------------------------------------------

func (p *pmox) lxcStatus(ctx context.Context, node string, vmid int) (*lxcStatus, error) {
	var st lxcStatus
	if err := p.cli.Get(ctx, lxcPath(node, vmid, "/status/current"), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// postLXCTask POSTs to a container endpoint that returns a task UPID and
// awaits it.
func (p *pmox) postLXCTask(ctx context.Context, vm *VM, suffix string, body any, seconds int) error {
	task, err := p.submitLXCTask(ctx, vm, suffix, body)
	if err != nil {
		return err
	}
	return p.awaitLXCTask(ctx, vm, suffix, task, seconds)
}

func (p *pmox) submitLXCTask(ctx context.Context, vm *VM, suffix string, body any) (*proxmox.Task, error) {
	var upid proxmox.UPID
	if err := p.cli.Post(ctx, lxcPath(vm.Node, vm.VMID, suffix), body, &upid); err != nil {
		return nil, err
	}
	if upid == "" {
		return nil, errors.New("Proxmox returned no task ID; outcome unknown")
	}
	if suffix == "/status/start" || suffix == "/status/reboot" {
		nodecap.ReportPacedTask(ctx, vm.Node, vm.VMID, string(upid), false)
		p.log.Debug("start: Proxmox task accepted", "vmid", vm.VMID, "node", vm.Node, "upid", upid, "operation", suffix)
	}
	return proxmox.NewTask(upid, p.cli), nil
}

func (p *pmox) awaitLXCTask(ctx context.Context, vm *VM, suffix string, task *proxmox.Task, seconds int) error {
	accepted := task.UPID
	err := awaitTask(ctx, task, seconds)
	if err == nil && (suffix == "/status/start" || suffix == "/status/reboot") {
		p.reportStartTaskOutcome(ctx, vm.Node, vm.VMID, accepted, task, suffix)
	}
	return err
}

// startLXC re-verifies the firewall sandbox before a paced HTTP POST.
func (p *pmox) startLXC(ctx context.Context, vm *VM, gate StartSubmission) error {
	if p.cfg.Firewall.Enabled {
		if err := p.ensureFirewallLXC(ctx, vm); err != nil {
			return fmt.Errorf("ensure firewall before start: %w", err)
		}
	}
	return p.startLXCTask(ctx, vm, gate)
}

func (p *pmox) startLXCTask(ctx context.Context, vm *VM, gates ...StartSubmission) error {
	ctx, span := tracer.Start(ctx, "provisioner.PowerOn")
	defer span.End()
	var task *proxmox.Task
	const operation = "/status/start"
	submit := func(requestCtx context.Context) error {
		ctx = requestCtx
		var err error
		task, err = p.submitLXCTask(requestCtx, vm, operation, nil)
		return err
	}
	var err error
	if len(gates) > 0 && gates[0] != nil {
		err = gates[0](ctx, submit)
	} else {
		err = submit(ctx)
	}
	if err == nil {
		err = p.awaitLXCTask(ctx, vm, operation, task, 300)
	}
	if err != nil && !isAlreadyRunning(err) {
		return fmt.Errorf("start ct: %w", err)
	}
	return nil
}

// stopLXC shuts the container down gracefully, letting PVE hard-stop it
// after lxcShutdownTimeoutSeconds (forceStop). A missing or already
// stopped container is success.
func (p *pmox) stopLXC(ctx context.Context, vm *VM) error {
	st, err := p.lxcStatus(ctx, vm.Node, vm.VMID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("get ct status: %w", classifyProxmoxError(err))
	}
	if st.Status != "running" {
		return nil
	}
	body := map[string]any{"timeout": lxcShutdownTimeoutSeconds, "forceStop": 1}
	if err := p.postLXCTask(ctx, vm, "/status/shutdown", body, lxcShutdownTimeoutSeconds+60); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("shutdown ct: %w", classifyProxmoxError(err))
	}
	return nil
}

// destroyLXC stops (if running) and purges a container. Idempotent: a
// missing container is success. purge=1 also drops it from backup /
// replication / HA configuration; force=1 covers a container that
// restarted between the stop and the delete.
func (p *pmox) destroyLXC(ctx context.Context, vm *VM) error {
	if err := p.stopLXC(ctx, vm); err != nil {
		p.log.Warn("stop before destroy failed; proceeding to delete anyway", "vmid", vm.VMID, "guest_type", config.GuestTypeLXC, "err", err)
	}
	var upid proxmox.UPID
	err := p.cli.DeleteWithParams(ctx, lxcPath(vm.Node, vm.VMID, ""), map[string]any{"purge": 1, "force": 1}, &upid)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete ct: %w", errIfProtected(classifyProxmoxError(err), "ct", vm.VMID))
	}
	if err := awaitTask(ctx, proxmox.NewTask(upid, p.cli), 120); err != nil {
		classified := classifyProxmoxError(err)
		if errors.Is(classified, ErrVMNotFound) {
			p.recentlyDestroyed.Set(vm.VMID, time.Now(), ttlcache.DefaultTTL)
			return nil
		}
		return fmt.Errorf("await delete ct: %w", errIfProtected(classified, "ct", vm.VMID))
	}
	p.recentlyDestroyed.Set(vm.VMID, time.Now(), ttlcache.DefaultTTL)
	return nil
}

// powerStateLXC returns the container's status. While InjectJITConfig is
// restarting the container it reports "running": the restart's brief
// stop is not job completion, and the power poller must not destroy the
// container mid-handoff.
func (p *pmox) powerStateLXC(ctx context.Context, vm *VM) (string, error) {
	if p.jitRestarts != nil && p.jitRestarts.Has(vm.VMID) {
		return "running", nil
	}
	st, err := p.lxcStatus(ctx, vm.Node, vm.VMID)
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return st.Status, nil
}

// waitReadyLXC polls the container until its status is "running". There
// is no guest agent in a container, so "ready" is weaker than for QEMU:
// the container is up, and its bootstrap unit idles until a JIT config
// arrives through the runtime environment.
func (p *pmox) waitReadyLXC(ctx context.Context, vm *VM, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		st, err := p.lxcStatus(waitCtx, vm.Node, vm.VMID)
		if err == nil && st.Status == "running" {
			return nil
		}
		if err != nil && isNotFound(err) {
			return fmt.Errorf("await ct running: %w", classifyProxmoxError(err))
		}
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("await ct %d running: timed out after %s", vm.VMID, timeout)
		case <-time.After(lxcReadyPollInterval):
		}
	}
}

// ---------------------------------------------------------------------------
// JIT delivery
// ---------------------------------------------------------------------------

// validateLXCEnvValue rejects anything PVE's env pattern would refuse or
// that could split the NUL-separated list: NUL, newline and other control
// characters. The JIT is base64 (already checked by jitConfigPattern);
// this keeps the guarantee local to the env encoding.
func validateLXCEnvValue(v string) error {
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 || c == 0x7f {
			return fmt.Errorf("value contains a control character at offset %d", i)
		}
	}
	return nil
}

// mergeLXCJITEnv returns the env option value with jit set: the existing
// NUL-separated entries in their original order minus any previous
// ACTIONS_RUNNER_JIT_CONFIG entry (and empty entries), then the new one.
func mergeLXCJITEnv(existing, jit string) string {
	var out []string
	for _, kv := range strings.Split(existing, "\x00") {
		if kv == "" || strings.HasPrefix(kv, lxcJITEnvKey+"=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, lxcJITEnvKey+"="+jit)
	return strings.Join(out, "\x00")
}

// errIfProtected adds an actionable message when PVE refused to delete a
// guest because its protection flag is set (typically inherited from a
// template carrying protection: 1). Protection is never cleared
// automatically: an operator set it, and it may be guarding a guest that
// is not ours.
func errIfProtected(err error, kind string, vmid int) error {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "protection") {
		return fmt.Errorf("%s %d has protection enabled; clear it on the guest (and on its template, so clones do not inherit it) before it can be destroyed: %w", kind, vmid, err)
	}
	return err
}

// redactJIT strips the JIT from an error that might echo request data.
func redactJIT(err error, jit string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if jit != "" && strings.Contains(msg, jit) {
		msg = strings.ReplaceAll(msg, jit, "[redacted]")
	}
	return errors.New(msg)
}

// injectLXCJIT delivers the JIT config to a container:
//
//  1. PUT /lxc/{id}/config env=ACTIONS_RUNNER_JIT_CONFIG=<jit>. On a
//     running container PVE stores it as a pending change.
//  2. Restart so init receives it: /status/reboot if running (PVE stops
//     the container, applies pending changes and starts it with the new
//     lxc.environment.runtime), /status/start otherwise.
//  3. Wait until the container reports running.
//
// The JIT never appears in logs or errors: request errors are redacted
// and no log line carries the value. While the restart is in flight the
// VMID sits in jitRestarts so PowerState masks the transient stop.
func (p *pmox) injectLXCJIT(ctx context.Context, vm *VM, jit string) error {
	if err := validateLXCEnvValue(jit); err != nil {
		return fmt.Errorf("inject jit: config for ct %d rejected: %w", vm.VMID, err)
	}
	if p.jitRestarts != nil {
		p.jitRestarts.Set(vm.VMID, time.Now(), ttlcache.DefaultTTL)
		defer p.jitRestarts.Delete(vm.VMID)
	}
	// Re-verify the sandbox before the restart, exactly as Start does: the
	// stopped-container branch below boots the container, and the reboot
	// branch is the boot that actually runs the job.
	if p.cfg.Firewall.Enabled {
		if err := p.ensureFirewallLXC(ctx, vm); err != nil {
			return fmt.Errorf("inject jit: ensure firewall on ct %d: %w", vm.VMID, err)
		}
	}
	// PVE's env option replaces the whole NUL-separated list, so merge:
	// keep the container's (template-inherited) variables in order, drop
	// any previous JIT entry and append the new one.
	stage := func() error {
		var ctCfg map[string]any
		if err := p.cli.Get(ctx, lxcPath(vm.Node, vm.VMID, "/config"), &ctCfg); err != nil {
			return fmt.Errorf("inject jit: read config of ct %d: %w", vm.VMID, classifyProxmoxError(err))
		}
		existing, _ := ctCfg["env"].(string)
		body := map[string]any{"env": mergeLXCJITEnv(existing, jit)}
		if err := p.cli.Put(ctx, lxcPath(vm.Node, vm.VMID, "/config"), body, nil); err != nil {
			return fmt.Errorf("inject jit: set env on ct %d: %w", vm.VMID, redactJIT(classifyProxmoxError(err), jit))
		}

		return nil
	}
	var task *proxmox.Task
	var operation string
	submit := func(ctx context.Context, status string) error {
		operation = "/status/start"
		var body any
		if status == "running" {
			operation = "/status/reboot"
			body = map[string]any{"timeout": lxcRebootTimeoutSeconds}
		}
		var err error
		task, err = p.submitLXCTask(ctx, vm, operation, body)
		if operation == "/status/start" && isAlreadyRunning(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inject jit: restart ct %d: %w", vm.VMID, redactJIT(classifyProxmoxError(err), jit))
		}
		return nil
	}
	releaseStart, powerCtx, err := p.awaitLXCJITStart(ctx, vm, stage, submit)
	defer nodecap.ReportPacedWorkerDone(powerCtx)
	if err != nil {
		return err
	}
	defer releaseStart()
	if task != nil {
		if err := p.awaitLXCTask(powerCtx, vm, operation, task, 300); err != nil {
			return fmt.Errorf("inject jit: await restart ct %d: %w", vm.VMID, redactJIT(classifyProxmoxError(err), jit))
		}
	}
	if err := p.waitReadyLXC(ctx, vm, lxcPostRestartReadyTimeout); err != nil {
		return fmt.Errorf("inject jit: ct %d did not come back: %w", vm.VMID, err)
	}
	nodecap.ReportPacedReady(powerCtx, vm.Name)
	p.log.Debug("jit delivered via container env", "vmid", vm.VMID, "node", vm.Node, "bytes", len(jit))
	return nil
}

// awaitLXCJITStart preserves the old path for installations without both
// limits, but a paced host sizes the current power state inside the gate.
func (p *pmox) awaitLXCJITStart(ctx context.Context, vm *VM, stage func() error, submit func(context.Context, string) error) (func(), context.Context, error) {
	gate, gateOK := p.startGate.(interface {
		UsesPacedAdmission() bool
		AcquireStartWithSubmission(context.Context, string, func(func()) error) (func(), error)
	})
	acct, acctOK := p.startCapacity.(interface {
		SupportsPacedStart() bool
		ReservePacedRestart(context.Context, string, int, time.Time) (nodecap.Reservation, bool, error)
		ReserveExistingForPacedStart(context.Context, string, nodecap.Shape, int, time.Time, bool) (nodecap.Reservation, bool, error)
	})
	if gateOK && acctOK && gate.UsesPacedAdmission() && acct.SupportsPacedStart() {
		notAdmitted := errors.New("fresh node capacity deferred")
		var status string
		powerCtx := ctx
		for {
			release, err := gate.AcquireStartWithSubmission(ctx, vm.Node, func(mark func()) error {
				st, err := p.lxcStatus(ctx, vm.Node, vm.VMID)
				if err != nil {
					if errors.Is(classifyProxmoxError(err), ErrVMNotFound) {
						return fmt.Errorf("inject jit: refresh ct %d power: %w", vm.VMID, ErrVMNotFound)
					}
					return notAdmitted
				}
				if st.Name == "" || st.Name != vm.Name {
					return notAdmitted
				}
				status = st.Status
				var claim nodecap.Reservation
				var admitted bool
				switch status {
				case "running":
					claim, admitted, err = acct.ReservePacedRestart(ctx, vm.Node, vm.VMID, vm.CreatedAt)
					if err != nil || !admitted {
						return ErrRunningJITCapacityDeferred
					}
				case "stopped":
					profile, ok := p.profiles[vm.Profile]
					if !ok || profile.MemoryMB <= 0 {
						return fmt.Errorf("inject jit: cannot size stopped ct %d", vm.VMID)
					}
					shape := nodecap.Shape{MemoryBytes: uint64(profile.MemoryMB) * 1024 * 1024, VCPUs: profile.CPUCores}
					claim, admitted, err = acct.ReserveExistingForPacedStart(ctx, vm.Node, shape, vm.VMID, vm.CreatedAt, false)
					if err != nil || !admitted {
						return notAdmitted
					}
				default:
					return notAdmitted
				}
				if err := stage(); err != nil {
					if claim != nil {
						claim.Release()
					}
					return err
				}
				if claim != nil {
					if starter, ok := claim.(interface{ BeginStart() bool }); ok && !starter.BeginStart() {
						claim.Release()
						// JIT has already been written. Never use the
						// safe-to-requeue result after that boundary.
						return errors.New("inject jit: staged before the capacity claim expired")
					}
					powerCtx = nodecap.WithPacedTaskReceipt(ctx, claim)
				}
				mark()
				defer nodecap.ReportPacedSubmissionReturned(powerCtx, vm.Name, "lxc")
				return submit(powerCtx, status)
			})
			if err == nil {
				return release, powerCtx, nil
			}
			if !errors.Is(err, notAdmitted) {
				return nil, powerCtx, err
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx, ctx.Err()
			case <-timer.C:
			}
		}
	}

	if err := stage(); err != nil {
		return nil, ctx, err
	}

	st, err := p.lxcStatus(ctx, vm.Node, vm.VMID)
	if err != nil {
		return nil, ctx, fmt.Errorf("inject jit: get ct %d status: %w", vm.VMID, classifyProxmoxError(err))
	}
	needsClaim := st.Status != "running" && p.startCapacity != nil
	var shape nodecap.Shape
	if needsClaim {
		profile, ok := p.profiles[vm.Profile]
		if !ok || profile.MemoryMB <= 0 {
			return nil, ctx, fmt.Errorf("inject jit: cannot size stopped ct %d", vm.VMID)
		}
		shape = nodecap.Shape{MemoryBytes: uint64(profile.MemoryMB) * 1024 * 1024, VCPUs: profile.CPUCores}
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, ctx, err
		}
		var claim nodecap.Reservation
		if needsClaim {
			var admitted bool
			if c, ok := p.startCapacity.(interface {
				ReserveExistingForJIT(context.Context, string, nodecap.Shape, int, time.Time) (nodecap.Reservation, bool, error)
			}); ok {
				claim, admitted, err = c.ReserveExistingForJIT(ctx, vm.Node, shape, vm.VMID, vm.CreatedAt)
			} else {
				claim, admitted, err = p.startCapacity.ReserveExisting(ctx, vm.Node, shape, vm.VMID)
			}
			if err != nil || !admitted {
				timer := time.NewTimer(time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx, ctx.Err()
				case <-timer.C:
				}
				continue
			}
		}
		release := func() {}
		if p.startGate != nil {
			var gateErr error
			release, gateErr = p.startGate.AcquireStart(ctx, vm.Node)
			if gateErr != nil {
				if claim != nil {
					claim.Release()
				}
				return nil, ctx, fmt.Errorf("inject jit: wait for host boot budget: %w", gateErr)
			}
		}
		if starter, ok := claim.(interface{ BeginStart() bool }); ok && !starter.BeginStart() {
			// A borrowed hold can expire while boot pacing is pending.
			// Drop the slot before attempting fresh admission; no POST
			// was issued, so this does not start the guest twice.
			release()
			claim.Release()
			continue
		}
		if err := submit(ctx, st.Status); err != nil {
			release()
			return nil, ctx, err
		}
		return release, ctx, nil
	}
}

// readLXCJIT returns the JIT config stored in the container's env option
// (pending value included, so a JIT set but not yet applied counts), or
// an empty slice when none is set. Same contract as the agent-backed
// variants: non-empty means the runner has been handed a JIT.
func (p *pmox) readLXCJIT(ctx context.Context, vm *VM) ([]byte, error) {
	var ctCfg map[string]any
	if err := p.cli.Get(ctx, lxcPath(vm.Node, vm.VMID, "/config"), &ctCfg); err != nil {
		return nil, fmt.Errorf("read ct %d config: %w", vm.VMID, classifyProxmoxError(err))
	}
	env, _ := ctCfg["env"].(string)
	for _, kv := range strings.Split(env, "\x00") {
		if v, ok := strings.CutPrefix(kv, lxcJITEnvKey+"="); ok {
			return []byte(v), nil
		}
	}
	return []byte{}, nil
}

// listContainers returns GET /nodes/{node}/lxc.
func (p *pmox) listContainers(ctx context.Context, node string) ([]lxcListEntry, error) {
	var out []lxcListEntry
	if err := p.cli.Get(ctx, fmt.Sprintf("/nodes/%s/lxc", node), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// truthy interprets a PVE boolean that may arrive as a number, string
// or bool.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t == "1" || t == "true"
	}
	return false
}
