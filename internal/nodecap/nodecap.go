// Package nodecap is the orchestrator's capacity accountant: it decides
// whether a Proxmox node has room for one more VM of a given shape, and
// hands out reservations so concurrent clone dispatches cannot spend the
// same megabyte twice.
//
// The hard constraint is ALLOCATED memory, not used memory. A booted-but-
// idle 16 GiB VM reports a low RSS while owning its full allocation, so
// admission must count what is promised to the guests on a node, never
// what they happen to be touching. (The least_loaded node selector's
// score deliberately uses USED memory — that is the right input for
// tie-breaking placement and the wrong input for admission. The two are
// not interchangeable.)
//
// "Allocated" means allocated ON THE HOST, which is not the same as
// configured. A guest that is powered off holds nothing — Proxmox
// reserves nothing for it — so counting its configured size would
// withhold capacity that physically exists. Only guests that actually
// hold memory count: every running guest, plus this orchestrator's own
// guests whatever their power state, since the warm tier is stopped by
// design and its memory is genuinely spoken for. See holdsMemory.
//
// Data source is a single GET /cluster/resources, which returns both the
// node rows (physical RAM / cores) and every guest row (qemu and lxc,
// including guests this orchestrator does not own) with the guest's
// configured maxmem, vcpu count, power status and tags. Proxmox derives
// maxmem and vcpus from the guest CONFIG rather than from a running
// process, so they describe what a guest is entitled to rather than what
// it is touching — which is exactly the "promised" number admission
// needs for the guests that count.
//
// All exported methods are safe for concurrent use.
package nodecap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/luthermonson/go-proxmox"
	"golang.org/x/sync/singleflight"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/tags"
)

// Shape is the resource footprint of one VM the orchestrator is about to
// clone. Built from the runner profile's declared cpu / memory_mb.
type Shape struct {
	MemoryBytes uint64
	VCPUs       int
}

// NodeState is the externally observable capacity of one node. Byte
// counts are absolute; Available is already net of both the host reserve
// and every outstanding reservation, so a caller can compare a Shape
// against it directly.
type NodeState struct {
	TotalBytes     uint64
	ReserveBytes   uint64 // host headroom withheld from admission
	CommittedBytes uint64 // guest allocations + outstanding reservations
	AvailableBytes uint64

	// MemoryPressured reports, in host_available mode, that the node is
	// withholding admission because it has no usable free-RAM reading:
	// the node's /status reading is missing from the snapshot, or the
	// snapshot is stale. It is the fail-closed signal an operator alerts
	// on; in the allocated mode (the default) it is always false.
	MemoryPressured bool

	// CPUGated reports whether either CPU admission mode is enabled. When
	// false, CPU is never a reason to refuse a clone and the three
	// fields below are informational only.
	CPUGated bool
	// LimitVCPU is the admission ceiling: logical CPUs times either the
	// allocated overcommit ratio or the host usage limit. AvailableVCPU is
	// the remaining headroom. In usage mode CommittedVCPU is measured CPU
	// equivalents plus pending launches, rather than allocated vCPUs.
	// Both are zero when CPUGated is false. (LimitVCPU is deliberately
	// not the raw core count: a consumer comparing a Shape against it
	// must see the same ceiling admission applies.)
	LimitVCPU     int
	CommittedVCPU int
	AvailableVCPU int

	// CPUUsageMode reports that CPU admission follows measured host
	// usage, and LaunchVCPU is what a launching clone is counted at in
	// that mode (see Options.CPULaunchVCPUs). Use LaunchVCPUs and FitsCPU
	// rather than comparing Shape.VCPUs against the fields above.
	CPUUsageMode bool
	LaunchVCPU   int
}

// LaunchVCPUs is the CPU a clone of shape is counted at while it
// launches. The allocated mode counts its configured cores. host_usage
// counts the configured launch footprint, capped at the shape's cores
// when those are smaller, because a runner that is booting or waiting
// for a job is close to idle and the measured reading takes over once it
// is running.
func (st NodeState) LaunchVCPUs(shape Shape) int {
	if !st.CPUUsageMode {
		return shape.VCPUs
	}
	if shape.VCPUs > 0 && shape.VCPUs < st.LaunchVCPU {
		return shape.VCPUs
	}
	return st.LaunchVCPU
}

// FitsCPU reports whether a clone of shape fits under the node's CPU
// ceiling. It compares committed plus the launch footprint against the
// limit, as the accountant does, so a node already over its ceiling
// refuses even a zero-vCPU launch.
func (st NodeState) FitsCPU(shape Shape) bool {
	return !st.CPUGated || st.CommittedVCPU+st.LaunchVCPUs(shape) <= st.LimitVCPU
}

// Reservation is a claim on a node's capacity held across a clone
// dispatch. The caller Binds it to the VMID once one has been allocated
// and Releases it if the clone fails before the VM exists. A reservation
// that reaches a live VM is never released explicitly — it retires
// itself once the accountant observes the VM at its full configured
// size (see the ledger notes on Accountant).
type Reservation interface {
	// Bind associates the reservation with the VMID that was minted for
	// it. Until this is called the reservation counts in full, because
	// no guest row can possibly account for it yet.
	Bind(vmid int)

	// Release drops the reservation. Idempotent — callers wire it into
	// a sync.OnceFunc and a defer, so the failure paths and the panic
	// guard can all call it.
	Release()

	// ExpectPoweredOff declares that the bound VM was created to stay
	// powered off (the warm tier). Such a VM consumes no host memory or
	// CPU, so the reservation retires as soon as the snapshot carries
	// the VM's row at full size, whatever the mode — instead of waiting
	// for a power-on that will not come and holding the full shape until
	// the TTL (host_available memory / host_usage CPU). The row itself
	// covers the VM where the mode counts stopped owned guests
	// (allocated), so there is no window in which it is counted by
	// neither. Starting the VM later reserves again.
	ExpectPoweredOff()
}

// Options configures an Accountant.
type Options struct {
	// Nodes restricts accounting to the operator-declared node list.
	// Empty means "every node the cluster reports".
	Nodes []string

	// Refresh is the TTL on the /cluster/resources snapshot.
	Refresh time.Duration

	// ReserveBytes and ReserveFraction are the host headroom withheld
	// from admission. The effective reserve for a node is the LARGER of
	// the two — the conservative reading of "absolute and/or fraction".
	ReserveBytes    uint64
	ReserveFraction float64

	// CPUOvercommit gates vCPU admission at (physical cores * ratio).
	// Zero disables CPU gating entirely: CPU is time-shared and
	// overcommittable, memory is not, so the default is permissive.
	CPUOvercommit float64

	// CPUMode defaults to allocated. host_usage uses measured host CPU with
	// CPUUsageLimit (0..1), counting each pending clone at CPULaunchVCPUs
	// (capped at its shape's cores) until it is observed powered on.
	CPUMode        string
	CPUUsageLimit  float64
	CPULaunchVCPUs int

	// MemoryMode selects how a node's usable memory is derived. The
	// zero value is MemoryModeAllocated (the default, and the historical
	// behaviour): admitted RAM is bounded by the sum of the guests' ALLOCATED
	// memory. MemoryModeHostAvailable instead admits against the node's
	// available RAM, as reported by /nodes/{node}/status — the number a
	// host can genuinely hand to a new guest — which also fails closed when
	// that reading is missing or stale. See MemoryMode.
	MemoryMode string

	// MemoryOvercommit scales how much of a reservation's RAM is withheld
	// from the node's free pool in MemoryModeHostAvailable. 1.0 (the
	// default) admits a clone only when free RAM covers its full request
	// plus the host reserve; a value below 1.0 lets the box be oversubscribed.
	// Ignored in the allocated mode, where memory is gated hard.
	MemoryOvercommit float64

	// ReservationTTL bounds how long a single reservation may survive
	// without retiring. It is a leak guard for a clone that hangs or
	// dies past the pool's panic recovery, not a normal expiry path.
	ReservationTTL time.Duration
	// WindowsOnlineSettleAfter enables a bounded early-release policy.
	WindowsOnlineSettleAfter time.Duration
	WindowsOnlineFreshness   time.Duration
	// RecoveryDelay pauses host-memory admission after controller startup,
	// allowing starts submitted by the previous process to settle.
	RecoveryDelay time.Duration

	// OwnedVMIDs are the VMID spans this orchestrator allocates clones
	// from — every scale set's range — and OwnerScaleSets are the scale
	// set names whose owner tag marks a VM as ours. Guests matching
	// EITHER count against capacity even when powered off, because the
	// warm tier is stopped by design; other guests count only while
	// running. See holdsMemory.
	//
	// Both are needed, because each covers a window the other misses.
	// Leaving them empty makes every stopped guest look foreign, which
	// would stop the pool's own warm VMs from counting. Callers that
	// run a warm pool must populate them.
	OwnedVMIDs     []VMIDRange
	OwnerScaleSets []string

	Log *slog.Logger
}

// VMIDRange is an inclusive span of Proxmox VMIDs. Mirrors
// config.VMIDRange; redeclared here so this package stays free of the
// config import.
type VMIDRange struct {
	Min int
	Max int
}

// CPU admission modes accepted by Options.CPUMode.
const (
	CPUModeAllocated = "allocated"
	CPUModeHostUsage = "host_usage"
)

// Memory modes accepted by Options.MemoryMode. See Options.MemoryMode.
const (
	// MemoryModeAllocated is the default and the historical behaviour: a
	// node's usable memory is total RAM minus the host reserve minus the
	// ALLOCATED memory of every guest that holds it. It does not consult
	// the node's live available RAM.
	MemoryModeAllocated = "allocated"

	// MemoryModeHostAvailable admits against the node's available RAM
	// (from /nodes/{node}/status) rather than the sum of the guests' allocated
	// memory. Because a host's free number is the only figure that already
	// folds in host overhead (PVE, ZFS ARC, qemu) and the in-flight startup
	// gap, it is the right input for "can this box hand a new guest its RAM".
	// It fails closed when a node's reading is missing or stale.
	MemoryModeHostAvailable = "host_available"
)

// guestStopped is the /cluster/resources status of a powered-off guest.
// Anything else — running, paused, or a status this version of PVE
// reports that we don't recognise — is treated as holding memory.
const guestStopped = "stopped"

// ErrNoSnapshot is returned when the accountant has never successfully
// read /cluster/resources. Admission fails closed in that window: with
// no idea what is allocated we cannot promise not to overcommit. It is a
// cold-start-only condition (once a snapshot lands, a later fetch error
// falls back to the stale one), and cloning is impossible anyway while
// the Proxmox API is unreachable.
var ErrNoSnapshot = errors.New("nodecap: no cluster resource snapshot available")

// defaults applied to zero-valued Options.
const (
	defaultRefresh        = 15 * time.Second
	defaultReservationTTL = 5 * time.Minute
	// fetchTimeout bounds the detached, singleflight-shared fetch. The
	// call is deliberately decoupled from any single caller's context
	// (so one caller's cancellation can't fail every waiter), so it
	// needs its own deadline. Mirrors nodeselector.fetchTimeout.
	fetchTimeout = 30 * time.Second
	// snapshotCacheKey is the single key in the snapshot cache. The
	// cache only ever holds one entry; ttlcache owns the TTL accounting.
	snapshotCacheKey = "resources"
)

// Accountant tracks per-node allocated capacity and hands out
// reservations.
//
// # The ledger
//
// One expression covers every phase of a clone's life:
//
//	committed(N) = Σ observedMem(g) for each non-template guest g on N
//	             + Σ max(0, r.mem - observedMem(r.vmid)) for each reservation r on N
//	             - Σ observedMem(v) for each v in r.freeing, for each r on N
//
// where observedMem(vmid) is the snapshot's maxmem for that VMID, or 0
// when it is absent. The three reservation phases fall out of the middle
// term automatically:
//
//   - unbound (no VMID minted yet): counts in full;
//   - bound, but the post-clone memory override has not landed: counts
//     the difference. This case is why a naive "drop the reservation
//     once the VMID appears" rule would undercount — qmclone copies the
//     TEMPLATE's memory and the provisioner raises it in a later Config
//     call, so for a few seconds the guest row understates the VM;
//   - bound and fully applied: counts zero, and is deleted on the next
//     refresh.
//
// Retirement is by OBSERVATION, not on clone success. Because
// /cluster/resources lags (pvestatd broadcasts on its own cadence),
// releasing a successful clone's reservation eagerly would open a window
// where the VM is absent from both the snapshot and the ledger. Deleting
// the reservation on retirement — rather than merely letting its term go
// to zero — matters too: once the VM is eventually destroyed its
// observedMem returns to 0, which would resurrect a merely-zeroed
// reservation.
type Accountant struct {
	cli  *proxmox.Client
	opts Options
	log  *slog.Logger

	// cache holds the most recent /cluster/resources read. Expired
	// reads return nil from Get; on miss we singleflight a fetch.
	cache *ttlcache.Cache[string, *snapshot]

	// lastFresh is the most recent successful snapshot regardless of
	// TTL. Consulted on the fetch-error path so a transient Proxmox
	// blip degrades to stale data — which, with reservations stacking
	// on top of it, drifts conservative — instead of stalling every
	// clone in the fleet.
	lastFresh *snapshot

	// sf collapses concurrent cache-miss fetches into one API call.
	sf singleflight.Group

	// mu guards lastFresh and res. It is also what makes the
	// fit-then-reserve sequence in reserve() atomic, which is the whole
	// no-double-spend guarantee: two concurrent dispatches for the last
	// 8 GiB cannot both observe room.
	mu sync.Mutex
	// res holds every outstanding reservation, keyed by a monotonic id.
	res     map[uint64]*reservation
	nextRes uint64
	// now is the clock used for reservation expiry and snapshot
	// staleness. Indirect so a test can age a snapshot without sleeping;
	// New pins it to time.Now.
	now           func() time.Time
	recoveryUntil time.Time
}

// snapshot is one immutable read of /cluster/resources.
type snapshot struct {
	// nodes maps node name to its physical totals and the sum of the
	// allocations of the guests it hosts.
	nodes map[string]nodeTotals
	// guestMem / guestVCPU map VMID to the guest's CONFIGURED footprint.
	// Keyed by VMID because that is what the reservation terms need to
	// look up. Templates are excluded — a template is a disk image, not
	// a live allocation.
	guestMem  map[int]uint64
	guestVCPU map[int]int
	// guestPowered maps VMID to whether the guest is powered on (its status
	// is anything other than stopped). host_available mode consults this:
	// a node's live available RAM reflects only powered-on guests, so a
	// reservation's RAM counts as "already in free" only once its guest is
	// powered on, not the moment its configured memory is raised.
	guestPowered map[int]bool
	qemuRunning  map[int]qemuIdentity
	takenAt      time.Time
}

type nodeTotals struct {
	memBytes uint64
	vcpus    int
	// guestMem / guestVCPU are the sums over this node's guests,
	// accumulated during fetch so evaluating the ledger is O(reservations)
	// rather than O(guests) per node.
	guestMem  uint64
	guestVCPU int
	// memAvailable is the node's live available RAM, read from /nodes/{node}/status
	// during fetch in host_available mode. memMissing is true when that
	// reading could not be taken (the API call failed or the node vanished)
	// and admission for the node must fail closed. Both are inert in the
	// allocated mode, which never consults the live reading.
	memAvailable uint64
	memReadAt    time.Time
	memMissing   bool
	cpuUsage     float64
	cpuMissing   bool
}

// reservation is one outstanding claim.
type reservation struct {
	id    uint64
	node  string
	shape Shape
	// vmid is 0 until Bind is called.
	vmid int
	// generation distinguishes a stored runner from a reuse of its VMID.
	generation time.Time
	// freeing lists VMIDs the caller has committed to destroying in
	// order to make this reservation fit. Their allocation is credited
	// back while they are still visible in the snapshot; each entry
	// drops out once its VMID leaves the snapshot (at which point the
	// snapshot already reflects the freed memory and crediting again
	// would double-count).
	freeing  []int
	created  time.Time
	expires  time.Time
	released bool
	acct     *Accountant
	// poweredOff marks a reservation for a VM that stays powered off;
	// it retires on observation alone (see ExpectPoweredOff).
	poweredOff bool
	started    bool
	startedAt  time.Time
	online     windowsOnlineProof
}

func (r *reservation) Bind(vmid int) {
	r.acct.mu.Lock()
	defer r.acct.mu.Unlock()
	r.vmid = vmid
}

// SetGeneration associates a claim with the exact store row it funds.
func (r *reservation) SetGeneration(createdAt time.Time) {
	r.acct.mu.Lock()
	defer r.acct.mu.Unlock()
	if !r.released && r.generation.IsZero() {
		r.generation = createdAt
	}
}

// borrowedStart uses an existing claim without transferring its ownership.
type borrowedStart struct{ original *reservation }

func (b borrowedStart) Bind(int)          {}
func (b borrowedStart) ExpectPoweredOff() {}
func (b borrowedStart) Release()          {}
func (b borrowedStart) BeginStart() bool  { return b.original.BeginStart() }

// BeginStart extends a live claim before issuing a start. A claim that has
// expired cannot be used to start a guest without re-admission.
func (r *reservation) BeginStart() bool {
	r.acct.mu.Lock()
	defer r.acct.mu.Unlock()
	if !r.acct.isHostMode() {
		return true
	}
	if r.released || time.Now().After(r.expires) {
		return false
	}
	now := time.Now()
	r.expires = now.Add(r.acct.opts.ReservationTTL)
	r.started = true
	r.startedAt = now
	r.online = windowsOnlineProof{}
	if r.acct.opts.WindowsOnlineSettleAfter > 0 && !r.generation.IsZero() {
		for _, previous := range r.acct.res {
			if previous != r && previous.node == r.node && previous.vmid == r.vmid &&
				previous.generation.Equal(r.generation) {
				previous.online = windowsOnlineProof{}
			}
		}
	}
	return true
}

// BeginRecovery pauses host-available admission at leader transitions.
func (a *Accountant) BeginRecovery(delay time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recoveryUntil = time.Now().Add(delay)
}

func (r *reservation) Release() {
	r.acct.mu.Lock()
	defer r.acct.mu.Unlock()
	if r.released {
		return
	}
	r.released = true
	delete(r.acct.res, r.id)
}

func (r *reservation) ExpectPoweredOff() {
	r.acct.mu.Lock()
	defer r.acct.mu.Unlock()
	r.poweredOff = true
}

// New constructs an Accountant over the given Proxmox client.
func New(cli *proxmox.Client, opts Options) (*Accountant, error) {
	if cli == nil {
		return nil, errors.New("nodecap: requires a non-nil proxmox client")
	}
	if opts.Refresh <= 0 {
		opts.Refresh = defaultRefresh
	}
	if opts.ReservationTTL <= 0 {
		opts.ReservationTTL = defaultReservationTTL
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Accountant{
		cli:           cli,
		opts:          opts,
		log:           opts.Log,
		now:           time.Now,
		recoveryUntil: time.Now().Add(opts.RecoveryDelay),
		cache: ttlcache.New[string, *snapshot](
			ttlcache.WithTTL[string, *snapshot](opts.Refresh),
			ttlcache.WithDisableTouchOnHit[string, *snapshot](),
		),
		res: make(map[uint64]*reservation),
	}, nil
}

// ReservationsBeforeDestroy captures the claims bound to this guest before
// issuing its destroy. Invoke the returned function only after confirmed
// destroy success, before releasing the pool's VMID. Claims acquired later
// are not affected, even if the VMID has been reused.
func (a *Accountant) ReservationsBeforeDestroy(node string, vmid int) func() {
	a.mu.Lock()
	var claims []*reservation
	for _, r := range a.res {
		if r.vmid == vmid && r.node == node {
			claims = append(claims, r)
		}
	}
	a.mu.Unlock()
	return func() {
		for _, r := range claims {
			r.Release()
		}
	}
}

// Snapshot returns the current per-node capacity state, net of the host
// reserve and every outstanding reservation. Callers use it to publish
// gauges and to size an eviction plan.
func (a *Accountant) Snapshot(ctx context.Context) (map[string]NodeState, error) {
	snap, err := a.resources(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked(snap)
	out := make(map[string]NodeState, len(snap.nodes))
	for node := range snap.nodes {
		out[node] = a.stateLocked(snap, node)
	}
	return out, nil
}

// Fits reports, for each candidate node, whether shape would be admitted
// there right now. Best effort and immediately stale — it narrows the
// node selector's candidate set, while Reserve remains the single atomic
// gate. A candidate the snapshot does not know (offline, removed, or
// outside the operator's declared list) is reported as not fitting.
func (a *Accountant) Fits(ctx context.Context, shape Shape, candidates []string) (map[string]bool, error) {
	snap, err := a.resources(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked(snap)
	out := make(map[string]bool, len(candidates))
	for _, node := range candidates {
		out[node] = a.fitsLocked(snap, node, shape, nil)
	}
	return out, nil
}

// Reserve atomically claims shape on node. The bool reports whether the
// shape fit; false is ordinary backpressure, not an error. An error means
// the accountant could not establish what is allocated at all.
func (a *Accountant) Reserve(ctx context.Context, node string, shape Shape) (Reservation, bool, error) {
	return a.ReserveFreeing(ctx, node, shape, nil)
}

// ReserveFreeing is Reserve with the allocation of freeing treated as
// already released. The pool's idle-eviction path uses it: it has just
// CAS'd those rows to Draining and queued their destroy, so their memory
// is committed to this reservation even though the snapshot will keep
// reporting them for a few more seconds.
//
// Taking the reservation at eviction time — rather than letting the freed
// memory return to the general pool — is what stops the victim's own
// profile from immediately reclaiming what it just lost.
func (a *Accountant) ReserveFreeing(ctx context.Context, node string, shape Shape, freeing []int) (Reservation, bool, error) {
	snap, err := a.resources(ctx)
	if err != nil {
		return nil, false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked(snap)
	if !a.fitsLocked(snap, node, shape, freeing) {
		return nil, false, nil
	}
	a.nextRes++
	r := &reservation{
		id:      a.nextRes,
		node:    node,
		shape:   shape,
		freeing: append([]int(nil), freeing...),
		created: time.Now(),
		expires: time.Now().Add(a.opts.ReservationTTL),
		acct:    a,
	}
	a.res[r.id] = r
	return r, true, nil
}

// ReserveExisting claims shape on node for an EXISTING, powered-off VM
// that is about to be started — the pool's warm-to-hot promotion. It is
// the same atomic gate as Reserve, so concurrent promotions and clones
// (from every profile and scale set sharing this accountant) cannot claim
// the same in-flight capacity twice. In host_available mode a start
// retains its claim throughout the bounded settling period.
//
// The reservation is bound to vmid from the start, and the fit check
// credits whatever that VM already contributes to the ledger, so a
// promotion is charged exactly once:
//   - allocated mode already counts an owned powered-off VM at its full
//     configured size (the warm tier is stopped by design), so the VM's
//     own row is credited back and only genuinely new demand is checked;
//   - host_available mode counts nothing for a powered-off guest, so the
//     full shape must fit in the node's free memory right now.
//
// The check is made even if the cached snapshot shows the VM powered on:
// that reading can be stale (a snapshot-recycled VM is back at Warm and
// stopped), and admitting on it would start the VM unchecked. For a VM
// that really is running this is merely conservative.
func (a *Accountant) ReserveExisting(ctx context.Context, node string, shape Shape, vmid int) (Reservation, bool, error) {
	return a.reserveExisting(ctx, node, shape, vmid, time.Time{})
}

// ReserveExistingForJIT lets a stopped container use a claim from its own
// cold start or warm promotion. It cannot borrow from another generation.
func (a *Accountant) ReserveExistingForJIT(ctx context.Context, node string, shape Shape, vmid int, generation time.Time) (Reservation, bool, error) {
	return a.reserveExisting(ctx, node, shape, vmid, generation)
}

func (a *Accountant) reserveExisting(ctx context.Context, node string, shape Shape, vmid int, generation time.Time) (Reservation, bool, error) {
	snap, err := a.resources(ctx)
	if err != nil {
		return nil, false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked(snap)
	if a.isHostMode() && vmid > 0 && !generation.IsZero() {
		for _, r := range a.res {
			if r.node != node || r.vmid != vmid || !r.generation.Equal(generation) ||
				r.shape != shape || !r.started || r.poweredOff || len(r.freeing) != 0 {
				continue
			}
			// The existing hold already covers this start. Do not count a
			// prospective destruction or start while below the guarded floor.
			totals, known := snap.nodes[node]
			reserve := a.reserveFor(totals.memBytes)
			holds, _ := a.holdsAndCreditsLocked(snap, node)
			if !known || totals.memMissing || a.stale(snap) || time.Now().Before(a.recoveryUntil) ||
				totals.memAvailable < reserve || holds > totals.memAvailable-reserve {
				return nil, false, nil
			}
			state := a.stateLocked(snap, node)
			if state.CPUGated && state.CommittedVCPU > state.LimitVCPU {
				return nil, false, nil
			}
			return borrowedStart{original: r}, true, nil
		}
	}
	if !a.fitsSelfLocked(snap, node, shape, vmid) {
		return nil, false, nil
	}
	a.nextRes++
	r := &reservation{
		id:         a.nextRes,
		node:       node,
		shape:      shape,
		vmid:       vmid,
		generation: generation,
		created:    time.Now(),
		expires:    time.Now().Add(a.opts.ReservationTTL),
		acct:       a,
	}
	a.res[r.id] = r
	return r, true, nil
}

// freeingCreditLocked is the memory a VM committed to destruction gives
// back. In host_available mode only a powered-on guest is in the node's
// live reading, so destroying a stopped one frees nothing admissible;
// crediting its configured size would admit memory that does not exist.
func (a *Accountant) freeingCreditLocked(snap *snapshot, vmid int) uint64 {
	if a.isHostMode() && !snap.guestPowered[vmid] {
		return 0
	}
	return snap.guestMem[vmid]
}

// fitsSelfLocked is fitsLocked for a VM that already exists on node:
// what the VM is already charged for (see ReserveExisting) is credited
// before shape is compared. Caller must hold a.mu.
func (a *Accountant) fitsSelfLocked(snap *snapshot, node string, shape Shape, vmid int) bool {
	if _, known := snap.nodes[node]; !known {
		return false
	}
	var memCredit uint64
	vcpuCredit := 0
	if !a.isHostMode() {
		memCredit = snap.guestMem[vmid]
	}
	if !a.cpuUsageMode() {
		vcpuCredit = snap.guestVCPU[vmid]
	}
	return a.fitsWithCreditLocked(snap, node, shape, memCredit, vcpuCredit)
}

// fitsLocked evaluates shape against node's remaining capacity. extraFreeing
// is credited on top of the ledger for the caller's own prospective
// reservation (the eviction path); pass nil for a plain fit check.
// Caller must hold a.mu.
func (a *Accountant) fitsLocked(snap *snapshot, node string, shape Shape, extraFreeing []int) bool {
	if _, known := snap.nodes[node]; !known {
		// Offline, removed, or outside the declared node list. Never
		// place onto a node we cannot account for.
		return false
	}
	credit := uint64(0)
	vcpuCredit := 0
	for _, vmid := range extraFreeing {
		credit += a.freeingCreditLocked(snap, vmid)
		if !a.cpuUsageMode() {
			vcpuCredit += snap.guestVCPU[vmid]
		}
	}
	return a.fitsWithCreditLocked(snap, node, shape, credit, vcpuCredit)
}

// fitsWithCreditLocked compares shape with node's remaining capacity
// after adding memCredit / vcpuCredit back — memory and vCPUs the caller
// knows are already charged to the ledger on its behalf. Caller must hold
// a.mu and must have checked node is in snap.
func (a *Accountant) fitsWithCreditLocked(snap *snapshot, node string, shape Shape, memCredit uint64, vcpuCredit int) bool {
	if a.isHostMode() && time.Now().Before(a.recoveryUntil) {
		return false
	}
	totals := snap.nodes[node]
	st := a.stateLocked(snap, node)
	avail := st.AvailableBytes + memCredit
	// In host_available mode the clone is admitted only when the node's
	// free RAM covers its request scaled by the overcommit factor (1.0 by
	// default = full RAM); the allocated mode gates on the full request.
	need := shape.MemoryBytes
	if a.isHostMode() {
		need = uint64(float64(shape.MemoryBytes) * a.memOvercommit())
	}
	if need > avail {
		return false
	}
	if a.cpuGated() {
		if st.CommittedVCPU-vcpuCredit+st.LaunchVCPUs(shape) > a.vcpuLimit(totals.vcpus) {
			return false
		}
	}
	return true
}

// stateLocked evaluates the ledger for one node. Caller must hold a.mu.
func (a *Accountant) stateLocked(snap *snapshot, node string) NodeState {
	totals := snap.nodes[node]
	reserve := a.reserveFor(totals.memBytes)
	committedVCPU := totals.guestVCPU
	if a.cpuUsageMode() {
		committedVCPU = int(math.Ceil(totals.cpuUsage * float64(totals.vcpus)))
		if totals.cpuMissing || a.stale(snap) {
			committedVCPU = a.vcpuLimit(totals.vcpus) + 1
		}
	}

	// CPU reservations withhold launch footprints until observation. Usage
	// mode does not credit allocated vCPUs for prospective evictions.
	// Signed: a freeing credit can exceed the pending claims, and unsigned
	// wraparound there would report a wildly empty node.
	deltaVCPU := 0
	for _, r := range a.res {
		if r.node != node {
			continue
		}
		observedVCPU := 0
		if r.vmid != 0 && (!a.cpuUsageMode() || (snap.takenAt.After(r.created) && snap.guestPowered[r.vmid])) {
			observedVCPU = snap.guestVCPU[r.vmid]
		}
		if launch := a.launchVCPUs(r.shape); launch > observedVCPU {
			deltaVCPU += launch - observedVCPU
		}
		for _, v := range r.freeing {
			if !a.cpuUsageMode() {
				deltaVCPU -= snap.guestVCPU[v]
			}
		}
	}
	committedVCPU += deltaVCPU
	if committedVCPU < 0 {
		committedVCPU = 0
	}

	var committed, available uint64

	if a.isHostMode() {
		// Fail closed: a node whose live available-memory reading is missing, or a
		// snapshot too old to trust, has no usable number to admit against.
		// Memory is not overcommittable, so guessing the other way would
		// silently oversubscribe the box.
		if totals.memMissing || a.stale(snap) {
			return NodeState{
				TotalBytes:      totals.memBytes,
				ReserveBytes:    reserve,
				CPUGated:        a.cpuGated(),
				CommittedVCPU:   committedVCPU,
				CPUUsageMode:    a.cpuUsageMode(),
				LaunchVCPU:      a.opts.CPULaunchVCPUs,
				MemoryPressured: true,
			}
		}
		// The node's available RAM already reflects every running guest (they are
		// physically consuming it), so the guest sum is NOT added as a base —
		// it would count them twice. Only in-flight reservations, whose RAM a
		// fresh measurement has not absorbed yet, are withheld on top.
		holds, credit := a.holdsAndCreditsLocked(snap, node)
		committed = saturatingSub(totals.memBytes, totals.memAvailable)
		usable := saturatingSub(totals.memAvailable, reserve)
		if usable > holds {
			available = usable - holds
		}
		if credit > 0 {
			available += credit
		}
	} else {
		// Allocated mode: the base is the sum of the guests' ALLOCATED memory
		// (the historical behaviour), and each reservation withholds the
		// shortfall between its full shape and what its VM row has absorbed.
		committed = totals.guestMem
		delta := int64(0)
		for _, r := range a.res {
			if r.node != node {
				continue
			}
			observed := uint64(0)
			if r.vmid != 0 {
				observed = snap.guestMem[r.vmid]
			}
			if r.shape.MemoryBytes > observed {
				//nolint:gosec // G115: a guest's configured memory is bounded by physical RAM, many orders of magnitude below 2^63 bytes (8 EiB).
				delta += int64(r.shape.MemoryBytes - observed)
			}
			for _, v := range r.freeing {
				//nolint:gosec // G115: as above — guest memory is bounded by physical RAM.
				delta -= int64(snap.guestMem[v])
			}
		}
		committed = addClamped(committed, delta)
		if usable := saturatingSub(totals.memBytes, reserve); usable > committed {
			available = usable - committed
		}
	}

	st := NodeState{
		TotalBytes:     totals.memBytes,
		ReserveBytes:   reserve,
		CommittedBytes: committed,
		AvailableBytes: available,
		CPUGated:       a.cpuGated(),
		CommittedVCPU:  committedVCPU,
		CPUUsageMode:   a.cpuUsageMode(),
		LaunchVCPU:     a.opts.CPULaunchVCPUs,
	}
	if st.CPUGated {
		st.LimitVCPU = a.vcpuLimit(totals.vcpus)
		if st.LimitVCPU > committedVCPU {
			st.AvailableVCPU = st.LimitVCPU - committedVCPU
		}
	}
	return st
}

// holdsAndCreditsLocked sums, for one node, the in-flight memory a ledger
// must withhold from the node's free pool (host_available mode) and the
// freeing credits to add back. Caller must hold a.mu.
func (a *Accountant) holdsAndCreditsLocked(snap *snapshot, node string) (holds, credit uint64) {
	for _, r := range a.res {
		if r.node != node {
			continue
		}
		observed := uint64(0)
		if r.vmid != 0 {
			observed = snap.guestMem[r.vmid]
			if a.isHostMode() && !r.poweredOff {
				// A powered-off guest's configured RAM is not in the node's
				// live available-memory reading, so treat the reservation as fully
				// withheld during the bounded settling interval; the power
				// state and configured RAM are not usage observations.
				observed = 0
			}
		}
		// In host mode a reservation withholds its full shape scaled by the
		// overcommit factor (a fresh measurement absorbs it once the guest's
		// RAM shows up in free), not the allocated shortfall.
		want := uint64(0)
		if a.isHostMode() {
			want = uint64(float64(r.shape.MemoryBytes) * a.memOvercommit())
		} else {
			want = r.shape.MemoryBytes
		}
		if want > observed {
			holds += want - observed
		}
		for _, v := range r.freeing {
			credit += a.freeingCreditLocked(snap, v)
		}
	}
	holds = saturatingSub(holds, a.windowsSettledCreditLocked(snap, node))
	return holds, credit
}

// isHostMode reports whether the accountant plans against live available RAM
// rather than the guests' allocated memory.
func (a *Accountant) isHostMode() bool {
	return a.opts.MemoryMode == MemoryModeHostAvailable
}

// memOvercommit is the host-mode memory overcommit factor, defaulting to
// 1.0 (no oversubscription) when unset.
func (a *Accountant) memOvercommit() float64 {
	if a.opts.MemoryOvercommit <= 0 {
		return 1.0
	}
	return a.opts.MemoryOvercommit
}

// stale reports whether a snapshot is too old to admit against in
// host_available mode: older than twice the refresh interval, which is
// the staleness an operator would call "not fresh" given the refresh cadence.
func (a *Accountant) stale(snap *snapshot) bool {
	if a.opts.Refresh <= 0 {
		return false
	}
	return a.now().Sub(snap.takenAt) > 2*a.opts.Refresh
}

func (a *Accountant) cpuUsageMode() bool { return a.opts.CPUMode == CPUModeHostUsage }

// launchVCPUs is NodeState.LaunchVCPUs for this accountant's options.
func (a *Accountant) launchVCPUs(shape Shape) int {
	return NodeState{CPUUsageMode: a.cpuUsageMode(), LaunchVCPU: a.opts.CPULaunchVCPUs}.LaunchVCPUs(shape)
}
func (a *Accountant) cpuGated() bool { return a.cpuUsageMode() || a.opts.CPUOvercommit > 0 }

// vcpuLimit is shared by admission and reported capacity.
func (a *Accountant) vcpuLimit(cores int) int {
	if a.cpuUsageMode() {
		limit := a.opts.CPUUsageLimit
		if limit == 0 {
			limit = 0.8
		}
		return int(float64(cores) * limit)
	}
	return int(float64(cores) * a.opts.CPUOvercommit)
}

// holdsMemory reports whether a guest in this state occupies host
// memory that admission must plan around.
//
// A POWERED-OFF guest occupies nothing. Proxmox reserves nothing for
// it, and counting its configured memory would withhold capacity that
// physically exists — on a host with a few dormant VMs that is enough
// to refuse every clone forever. "Allocated, not used" is about a
// running guest that is idle (it owns its full allocation while
// touching little of it); a stopped guest owns nothing at all.
//
// The orchestrator's OWN guests are the exception and always count,
// whatever their power state, because the warm tier is stopped BY
// DESIGN — a warm VM is pre-cloned and powered off precisely so it can
// boot on demand. Skipping those would let the pool clone warm VMs
// without limit, none of them counting, and then oversubscribe the node
// the moment they start.
//
// The residual risk is an operator starting a large dormant VM while
// runners hold the memory it wants. That is what reserve_memory_mb is
// for: withhold headroom for guests you know may wake up.
func (a *Accountant) holdsMemory(status, guestTags string, vmid int) bool {
	if status != guestStopped {
		return true
	}
	return a.owns(guestTags, vmid)
}

// owns reports whether a guest belongs to this orchestrator.
//
// Two independent tests, because each covers a window the other misses:
//
//   - The OWNER TAG is what the rest of the orchestrator means by
//     ownership — provisioner.classifyVM adopts any tagged VM whatever
//     its VMID, so a range test alone would call an owned VM foreign
//     after an operator repartitions vmid_range. Its stopped warm VMs
//     would then stop counting, the pool would clone a node's worth of
//     runners on top of them, and the node would be oversubscribed the
//     moment one got promoted.
//   - The VMID RANGE covers the opposite gap: a fresh clone carries its
//     VMID from the instant it exists, while its tags land in a
//     follow-up API call. During that window a tag test would call our
//     own clone foreign — and would keep doing so after its reservation
//     retired, which is exactly when the guest row becomes the only
//     thing accounting for it.
func (a *Accountant) owns(guestTags string, vmid int) bool {
	for _, name := range a.opts.OwnerScaleSets {
		if tags.IsOwnedBy(guestTags, name) {
			return true
		}
	}
	for _, r := range a.opts.OwnedVMIDs {
		if vmid >= r.Min && vmid <= r.Max {
			return true
		}
	}
	return false
}

// reserveFor is the host headroom withheld on a node with the given
// physical memory: the larger of the absolute and fractional settings.
func (a *Accountant) reserveFor(total uint64) uint64 {
	frac := uint64(float64(total) * a.opts.ReserveFraction)
	if frac > a.opts.ReserveBytes {
		return frac
	}
	return a.opts.ReserveBytes
}

// sweepLocked retires reservations against a fresh snapshot and drops
// expired ones. Caller must hold a.mu.
//
// A reservation retires once its VM is observed at (or above) its full
// configured size in allocated mode. In host_available mode a powered
// guest remains reserved for a bounded settling period because the power
// flag does not show whether host memory usage has caught up. Expiry is
// also the leak guard for clones that never got that far.
func (a *Accountant) sweepLocked(snap *snapshot) {
	now := time.Now()
	for id, r := range a.res {
		if now.After(r.expires) {
			if a.isHostMode() && r.started {
				a.log.Debug("nodecap: start reservation settled", "node", r.node, "vmid", r.vmid)
			} else {
				a.log.Warn("nodecap: reservation expired without retiring; releasing",
					"node", r.node, "vmid", r.vmid, "memory_bytes", r.shape.MemoryBytes)
			}
			r.released = true
			delete(a.res, id)
			continue
		}
		retired := r.vmid != 0 && snap.guestMem[r.vmid] >= r.shape.MemoryBytes
		if a.isHostMode() && !r.poweredOff {
			// Power-on is not evidence that host RAM has settled. Expiry
			// above is the bounded settling policy for these reservations.
			retired = false
		}
		if (a.isHostMode() || a.cpuUsageMode()) && !r.poweredOff {
			// In host mode the node's live available-memory reading absorbs a
			// reservation's RAM only once its guest is actually powered on.
			// A powered-off guest's configured RAM never reached that reading,
			// so retiring now would stop withholding RAM the host is not yet
			// spending.
			retired = retired && snap.takenAt.After(r.created) && snap.guestPowered[r.vmid]
		}
		if retired {
			r.released = true
			delete(a.res, id)
			continue
		}
		// Drop freeing credits whose VMID has left the snapshot: the
		// snapshot now reflects the freed memory on its own, and
		// crediting it a second time would over-report capacity.
		if len(r.freeing) > 0 {
			kept := r.freeing[:0]
			for _, v := range r.freeing {
				if _, still := snap.guestMem[v]; still {
					kept = append(kept, v)
				}
			}
			r.freeing = kept
		}
	}
}

// resources returns the current snapshot, fetching on cache miss.
// Mirrors nodeselector.leastLoaded.scores: singleflight so a burst of
// concurrent dispatches makes one API call, and a stale fallback so a
// transient Proxmox error doesn't stall admission fleet-wide.
func (a *Accountant) resources(ctx context.Context) (*snapshot, error) {
	if item := a.cache.Get(snapshotCacheKey); item != nil {
		return item.Value(), nil
	}
	v, err, _ := a.sf.Do(snapshotCacheKey, func() (any, error) {
		// Detached from the triggering caller's ctx: this fetch is
		// shared by every concurrent waiter, so one caller abandoning
		// it must not fail the rest. WithoutCancel keeps ctx values
		// (tracing) but drops cancellation; the timeout bounds it.
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		defer cancel()
		fresh, err := a.fetch(fetchCtx)
		if err != nil {
			return nil, err
		}
		a.cache.Set(snapshotCacheKey, fresh, ttlcache.DefaultTTL)
		a.mu.Lock()
		a.lastFresh = fresh
		a.mu.Unlock()
		return fresh, nil
	})
	if err != nil {
		a.mu.Lock()
		prev := a.lastFresh
		a.mu.Unlock()
		if prev != nil {
			a.log.Debug("nodecap: resource fetch failed; using last known-good snapshot",
				"age", time.Since(prev.takenAt), "err", err)
			return prev, nil
		}
		return nil, fmt.Errorf("%w: %w", ErrNoSnapshot, err)
	}
	return v.(*snapshot), nil
}

// fetch reads GET /cluster/resources and projects it into a snapshot.
//
// The raw client Get is used rather than Client.Cluster().Resources()
// because the latter issues an extra /cluster/status round-trip per call
// that carries nothing this package needs.
func (a *Accountant) fetch(ctx context.Context) (*snapshot, error) {
	var rs proxmox.ClusterResources
	if err := a.cli.Get(ctx, "/cluster/resources", &rs); err != nil {
		return nil, fmt.Errorf("list cluster resources: %w", err)
	}
	hostMode := a.isHostMode()
	snap := &snapshot{
		nodes:        map[string]nodeTotals{},
		guestMem:     map[int]uint64{},
		guestVCPU:    map[int]int{},
		guestPowered: map[int]bool{},
		takenAt:      a.now(),
	}
	if a.opts.WindowsOnlineSettleAfter > 0 {
		snap.qemuRunning = make(map[int]qemuIdentity)
	}
	want := func(name string) bool {
		return len(a.opts.Nodes) == 0 || slices.Contains(a.opts.Nodes, name)
	}
	// Guest rows and node rows arrive in one undifferentiated list with
	// no ordering guarantee, so guests are accumulated separately and
	// folded into the node totals afterwards.
	guestSums := map[string]nodeTotals{}
	for _, r := range rs {
		switch r.Type {
		case "node":
			// An offline node reports a non-online status (and stale or
			// zero totals). Leaving it out of snap.nodes means
			// fitsLocked refuses to place there, which is what we want.
			if !want(r.Node) || (r.Status != "" && r.Status != "online") {
				continue
			}
			snap.nodes[r.Node] = nodeTotals{
				memBytes: r.MaxMem,
				vcpus:    int(r.MaxCPU), //nolint:gosec // physical core counts are far below int range
			}
		case "qemu", "lxc":
			// Templates own no memory — they are disk images. Foreign
			// guests that DO hold memory still count: a node's real
			// capacity is its total minus everything allocated on it,
			// not just minus this orchestrator's own VMs.
			if !want(r.Node) || r.Template != 0 {
				continue
			}
			vmid := int(r.VMID) //nolint:gosec // VMIDs are bounded well below int range
			// Record power state before the allocation filter so the map is a
			// faithful record of who is actually consuming live host RAM — the
			// thing host_available mode keys its holds and retirement on.
			snap.guestPowered[vmid] = r.Status != guestStopped
			if snap.qemuRunning != nil && r.Type == "qemu" && r.Status == "running" {
				snap.qemuRunning[vmid] = qemuIdentity{node: r.Node, name: r.Name, uptime: r.Uptime}
			}
			if !a.holdsMemory(r.Status, r.Tags, vmid) {
				continue
			}
			vcpu := int(r.MaxCPU) //nolint:gosec // vcpu counts are far below int range
			snap.guestMem[vmid] = r.MaxMem
			snap.guestVCPU[vmid] = vcpu
			sum := guestSums[r.Node]
			sum.guestMem += r.MaxMem
			sum.guestVCPU += vcpu
			guestSums[r.Node] = sum
		}
	}
	for node, totals := range snap.nodes {
		totals.guestMem = guestSums[node].guestMem
		totals.guestVCPU = guestSums[node].guestVCPU
		if hostMode || a.cpuUsageMode() {
			// Admission plans against the node's live available RAM, which only the
			// per-node status endpoint reports. A failed reading marks the node
			// as missing so stateLocked fails closed rather than admit blind.
			totals.memReadAt = a.now()
			st, err := a.nodeLiveStatus(ctx, node)
			free, ok := st.availableMemory()
			totals.memAvailable = free
			totals.memMissing = err != nil || !ok
			totals.cpuMissing = err != nil || st.CPU == nil
			if st.CPU != nil {
				totals.cpuUsage = *st.CPU
				totals.cpuMissing = totals.cpuMissing || math.IsNaN(*st.CPU) || math.IsInf(*st.CPU, 0) || *st.CPU < 0 || *st.CPU > 1
			}
		}
		snap.nodes[node] = totals
	}
	return snap, nil
}

// nodeLiveStatus supplies both admission modes in one API round trip.
type liveStatus struct {
	CPU    *float64 `json:"cpu"`
	Memory struct {
		Available *uint64 `json:"available"`
		Used      *uint64 `json:"used"`
		Total     uint64  `json:"total"`
	} `json:"memory"`
}

func (a *Accountant) nodeLiveStatus(ctx context.Context, node string) (liveStatus, error) {
	var st liveStatus
	err := a.cli.Get(ctx, "/nodes/"+node+"/status", &st)
	if err != nil {
		a.log.Warn("nodecap: node status read failed; node will fail closed this refresh", "node", node, "err", err)
	}
	return st, err
}

// Older APIs omit available but report used excluding reclaimable cache.
func (st liveStatus) availableMemory() (uint64, bool) {
	if st.Memory.Total == 0 {
		return 0, false
	}
	if st.Memory.Available != nil {
		return *st.Memory.Available, *st.Memory.Available <= st.Memory.Total
	}
	if st.Memory.Used == nil || *st.Memory.Used > st.Memory.Total {
		return 0, false
	}
	return st.Memory.Total - *st.Memory.Used, true
}

// addClamped adds a signed delta to an unsigned base, flooring at 0.
func addClamped(base uint64, delta int64) uint64 {
	if delta >= 0 {
		return base + uint64(delta)
	}
	return saturatingSub(base, uint64(-delta))
}

func saturatingSub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}
