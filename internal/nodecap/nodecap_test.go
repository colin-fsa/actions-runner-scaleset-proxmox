package nodecap

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/tags"
)

const gib = 1024 * 1024 * 1024

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// resourceServer serves GET /cluster/resources from a swappable payload
// so a test can change what Proxmox reports between calls (a clone
// landing, a VM being destroyed, the API going down).
type resourceServer struct {
	*httptest.Server
	mu      sync.Mutex
	rows    []map[string]any
	failing bool
	calls   atomic.Int64
	// nodeStatus maps a node name to its GET /nodes/{node}/status payload.
	// The host_available mode reads memory.free from here. A node with no
	// entry errors (500), which the accountant treats as a missing reading
	// and fails closed for.
	nodeStatus map[string]map[string]any
}

func newResourceServer(t *testing.T, rows []map[string]any) *resourceServer {
	t.Helper()
	rs := &resourceServer{rows: rows}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.calls.Add(1)
		if node, ok := nodeStatusPath(r.URL.Path); ok {
			rs.serveNodeStatus(w, node)
			return
		}
		rs.mu.Lock()
		failing, rows := rs.failing, rs.rows
		rs.mu.Unlock()
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "proxmox is having a moment")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]any{"data": rows})
	}))
	t.Cleanup(rs.Close)
	return rs
}

// nodeStatusPath reports whether the path is a GET /nodes/{node}/status and
// returns the node name. The host_available mode reads each node's live
// free RAM from this endpoint.
func nodeStatusPath(path string) (string, bool) {
	const prefix = "/nodes/"
	const suffix = "/status"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	node := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if node == "" {
		return "", false
	}
	return node, true
}

func (rs *resourceServer) serveNodeStatus(w http.ResponseWriter, node string) {
	rs.mu.Lock()
	payload, ok := rs.nodeStatus[node]
	rs.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		// 500, not 404: go-proxmox only surfaces 5xx/400 as errors, so a
		// 404 would unmarshal to a spurious free=0. A 500 makes the
		// accountant see a failed read and fail closed.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"no such node"}`)
		return
	}
	writeJSON(w, map[string]any{"data": payload})
}

// setNodeStatus pins a node's /status reading. The host_available mode
// admits against free, bounded by total; call it for every node the
// accountant must plan against in host mode.
func (rs *resourceServer) setNodeStatus(node string, free, total uint64) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.nodeStatus == nil {
		rs.nodeStatus = map[string]map[string]any{}
	}
	rs.nodeStatus[node] = map[string]any{
		"memory": map[string]any{"free": free, "total": total, "used": total - free},
	}
}

func (rs *resourceServer) setRows(rows []map[string]any) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.rows = rows
}

func (rs *resourceServer) setFailing(v bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.failing = v
}

func writeJSON(w http.ResponseWriter, v any) {
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func nodeRow(name string, memBytes uint64, cpus int) map[string]any {
	return map[string]any{
		"id": "node/" + name, "type": "node", "node": name,
		"status": "online", "maxmem": memBytes, "maxcpu": cpus,
	}
}

// guestRow is a RUNNING guest — one that actually holds host memory.
func guestRow(vmid int, node string, memBytes uint64, vcpus int) map[string]any {
	return map[string]any{
		"id": fmt.Sprintf("qemu/%d", vmid), "type": "qemu", "node": node,
		"vmid": vmid, "status": "running", "maxmem": memBytes, "maxcpu": vcpus,
	}
}

// ownerTags renders the wire tag string PVE would report for a guest
// owned by the named scale set. Built through the tags package so the
// fixture cannot drift from the real encoding.
func ownerTags(t *testing.T, scaleSet string) string {
	t.Helper()
	owner, err := tags.OwnerTag(scaleSet)
	require.NoError(t, err)
	return tags.Marker + ";" + owner
}

// stoppedGuestRow is a powered-off guest. It holds nothing, so whether
// it counts depends entirely on whether it is one of ours.
func stoppedGuestRow(vmid int, node string, memBytes uint64, vcpus int) map[string]any {
	r := guestRow(vmid, node, memBytes, vcpus)
	r["status"] = "stopped"
	return r
}

func newAccountant(t *testing.T, rs *resourceServer, mutate func(*Options)) *Accountant {
	t.Helper()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12} //nolint:gosec // plain-http test server
	cli := proxmox.NewClient(rs.URL, proxmox.WithHTTPClient(&http.Client{Transport: tr}))

	opts := Options{
		Refresh:        time.Millisecond,
		ReservationTTL: time.Minute,
		// The orchestrator's own clone range, as app.Run wires it. VMIDs
		// outside this are foreign; the fixtures use 100-999 for those.
		OwnedVMIDs: []VMIDRange{{Min: 10000, Max: 10999}},
		Log:        quietLogger(),
	}
	if mutate != nil {
		mutate(&opts)
	}
	a, err := New(cli, opts)
	require.NoError(t, err)
	return a
}

// settle waits out the (deliberately tiny) snapshot TTL so the next call
// re-reads the server. Tests that change what Proxmox reports call it to
// make the change visible.
func settle() { time.Sleep(5 * time.Millisecond) }

// TestCommittedCountsAllocationNotUsage is the premise of the whole
// package: a RUNNING 16 GiB VM owns its full allocation even while it
// sits idle touching almost none of it. Sizing against its resident set
// instead is exactly the overcommit this feature exists to prevent.
func TestCommittedCountsAllocationNotUsage(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{
		nodeRow("pve1", 32*gib, 16),
		// Running but idle: PVE would report a small mem/cpu here.
		// maxmem comes from the config, and that is what must bind.
		guestRow(101, "pve1", 16*gib, 8),
	})
	a := newAccountant(t, rs, nil)

	states, err := a.Snapshot(context.Background())
	require.NoError(t, err)
	st := states["pve1"]
	require.Equal(t, uint64(16*gib), st.CommittedBytes,
		"an idle running VM's configured memory must count as committed")
	// 32 GiB total, no host reserve configured in this test, minus the
	// 16 GiB the idle guest owns.
	require.Equal(t, uint64(16*gib), st.AvailableBytes)
}

// TestForeignAndTemplateGuests: guests this orchestrator does not own
// consume real memory and must count; templates are disk images and
// must not.
func TestForeignAndTemplateGuests(t *testing.T) {
	t.Parallel()
	tpl := guestRow(9000, "pve1", 8*gib, 4)
	tpl["template"] = 1
	lxc := guestRow(200, "pve1", 4*gib, 2)
	lxc["type"] = "lxc"

	rs := newResourceServer(t, []map[string]any{
		nodeRow("pve1", 32*gib, 16),
		guestRow(101, "pve1", 8*gib, 4), // a foreign VM
		lxc,                             // a foreign container
		tpl,                             // our template
	})
	a := newAccountant(t, rs, nil)

	states, err := a.Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(12*gib), states["pve1"].CommittedBytes,
		"foreign qemu + lxc guests count; the template does not")
}

// TestStoppedForeignGuestHoldsNothing: a powered-off VM occupies no
// host memory and Proxmox reserves nothing for it, so counting its
// configured size withholds capacity that physically exists. On a host
// with a few dormant VMs that is the difference between a working pool
// and one that refuses every clone forever.
func TestStoppedForeignGuestHoldsNothing(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{
		nodeRow("pve1", 32*gib, 16),
		guestRow(101, "pve1", 8*gib, 4),         // running: holds its 8 GiB
		stoppedGuestRow(400, "pve1", 16*gib, 8), // powered off: holds nothing
	})
	a := newAccountant(t, rs, nil)

	states, err := a.Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(8*gib), states["pve1"].CommittedBytes,
		"only the running guest's allocation is real")
	require.Equal(t, uint64(24*gib), states["pve1"].AvailableBytes,
		"the dormant VM's 16 GiB must not be withheld")
}

// TestStoppedOwnedGuestStillCounts is the other half, and the reason
// this cannot simply be "skip stopped guests": the warm tier is stopped
// BY DESIGN. A warm VM is pre-cloned and powered off precisely so it can
// boot on demand, so its memory is spoken for. Skip those and the pool
// would clone warm VMs without limit — none counting — then oversubscribe
// the node the instant they start.
func TestStoppedOwnedGuestStillCounts(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{
		nodeRow("pve1", 32*gib, 16),
		stoppedGuestRow(10001, "pve1", 8*gib, 4), // our warm VM
		stoppedGuestRow(10002, "pve1", 8*gib, 4), // our warm VM
		stoppedGuestRow(400, "pve1", 16*gib, 8),  // someone else's, dormant
	})
	a := newAccountant(t, rs, nil) // OwnedVMIDs defaults to 10000-10999

	states, err := a.Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(16*gib), states["pve1"].CommittedBytes,
		"both warm VMs count; the foreign dormant one does not")
}

// TestStoppedOwnedGuestCountsByTagOutsideTheRange. Ownership by VMID
// range alone is not enough: the rest of the orchestrator owns VMs by
// TAG — provisioner.classifyVM adopts any tagged VM whatever its VMID —
// so a range repartition leaves owned VMs sitting outside the new
// ranges. Their stopped warm capacity must keep counting, or the pool
// clones a node's worth of runners on top of them and oversubscribes
// the moment one is promoted.
func TestStoppedOwnedGuestCountsByTagOutsideTheRange(t *testing.T) {
	t.Parallel()
	// 9001 is ours by tag but predates the current 10000-10999 range.
	legacy := stoppedGuestRow(9001, "pve1", 16*gib, 8)
	legacy["tags"] = ownerTags(t, "prod")

	rs := newResourceServer(t, []map[string]any{
		nodeRow("pve1", 64*gib, 16),
		legacy,
		stoppedGuestRow(400, "pve1", 8*gib, 4), // genuinely foreign
	})
	a := newAccountant(t, rs, func(o *Options) { o.OwnerScaleSets = []string{"prod"} })

	states, err := a.Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(16*gib), states["pve1"].CommittedBytes,
		"an owned VM outside the VMID range still holds its warm allocation")
}

// TestStoppedGuestWithForeignTagIsNotOurs guards the other direction:
// a tag belonging to some other tool, or another orchestrator's scale
// set, must not be mistaken for ours.
func TestStoppedGuestWithForeignTagIsNotOurs(t *testing.T) {
	t.Parallel()
	other := stoppedGuestRow(400, "pve1", 16*gib, 8)
	other["tags"] = ownerTags(t, "someone-else")

	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 64*gib, 16), other})
	a := newAccountant(t, rs, func(o *Options) { o.OwnerScaleSets = []string{"prod"} })

	states, err := a.Snapshot(context.Background())
	require.NoError(t, err)
	require.Zero(t, states["pve1"].CommittedBytes,
		"another owner's dormant VM holds no memory of ours to plan around")
}

// TestUnknownGuestStatusCountsConservatively: only an explicit "stopped"
// is treated as holding nothing. A paused guest, or a status a future
// PVE reports that we don't recognise, must count — guessing the other
// way would silently overcommit.
func TestUnknownGuestStatusCountsConservatively(t *testing.T) {
	t.Parallel()
	paused := guestRow(101, "pve1", 8*gib, 4)
	paused["status"] = "paused"
	surprising := guestRow(102, "pve1", 4*gib, 2)
	surprising["status"] = "hibernating"

	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16), paused, surprising})
	a := newAccountant(t, rs, nil)

	states, err := a.Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(12*gib), states["pve1"].CommittedBytes,
		"anything that isn't definitively stopped counts")
}

// TestHostReserveTakesTheLarger pins the "absolute and/or fraction"
// semantics: the effective reserve is whichever is bigger.
func TestHostReserveTakesTheLarger(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 100*gib, 16)})

	absWins := newAccountant(t, rs, func(o *Options) {
		o.ReserveBytes = 20 * gib
		o.ReserveFraction = 0.1 // 10 GiB
	})
	states, err := absWins.Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(20*gib), states["pve1"].ReserveBytes)

	fracWins := newAccountant(t, rs, func(o *Options) {
		o.ReserveBytes = 4 * gib
		o.ReserveFraction = 0.25 // 25 GiB
	})
	states, err = fracWins.Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(25*gib), states["pve1"].ReserveBytes)
	require.Equal(t, uint64(75*gib), states["pve1"].AvailableBytes)
}

// TestReserveRefusesOnceFull walks the homelab shape: a 32 GiB node
// (4 GiB reserved) takes 16 + 8 + 4 and then refuses the next 8.
func TestReserveRefusesOnceFull(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
	a := newAccountant(t, rs, func(o *Options) { o.ReserveBytes = 4 * gib })
	ctx := context.Background()

	for _, memGiB := range []uint64{16, 8, 4} {
		_, ok, err := a.Reserve(ctx, "pve1", Shape{MemoryBytes: memGiB * gib})
		require.NoError(t, err)
		require.True(t, ok, "%d GiB should still fit", memGiB)
	}
	_, ok, err := a.Reserve(ctx, "pve1", Shape{MemoryBytes: 8 * gib})
	require.NoError(t, err)
	require.False(t, ok, "the node is full; the 4th reservation must be refused")

	// ...and the refusal is not an error, it is backpressure.
	states, err := a.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(28*gib), states["pve1"].CommittedBytes)
	require.Zero(t, states["pve1"].AvailableBytes)
}

// TestReleaseReturnsCapacity: a clone that fails before the VM exists
// hands its claim straight back rather than waiting out the TTL.
func TestReleaseReturnsCapacity(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 16*gib, 8)})
	a := newAccountant(t, rs, func(o *Options) { o.ReserveBytes = 0 })
	ctx := context.Background()

	res, ok, err := a.Reserve(ctx, "pve1", Shape{MemoryBytes: 16 * gib})
	require.NoError(t, err)
	require.True(t, ok)

	_, ok, err = a.Reserve(ctx, "pve1", Shape{MemoryBytes: 1 * gib})
	require.NoError(t, err)
	require.False(t, ok, "the whole node is claimed")

	res.Release()
	res.Release() // idempotent — callers wire this into a defer AND the failure paths

	_, ok, err = a.Reserve(ctx, "pve1", Shape{MemoryBytes: 1 * gib})
	require.NoError(t, err)
	require.True(t, ok, "released capacity must be immediately reusable")
}

// TestReservationSurvivesPartialClone is the subtle one. qmclone copies
// the TEMPLATE's memory; the provisioner raises it to the profile's
// memory in a LATER config call. In between, the guest row understates
// the VM — so a "drop the reservation once the VMID appears" rule would
// undercount and let a second clone in on memory that is already spoken
// for.
func TestReservationSurvivesPartialClone(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
	a := newAccountant(t, rs, func(o *Options) { o.ReserveBytes = 0 })
	ctx := context.Background()

	res, ok, err := a.Reserve(ctx, "pve1", Shape{MemoryBytes: 16 * gib})
	require.NoError(t, err)
	require.True(t, ok)
	res.Bind(10001)

	// Phase 1: the clone exists but still carries the template's 2 GiB.
	rs.setRows([]map[string]any{nodeRow("pve1", 32*gib, 16), guestRow(10001, "pve1", 2*gib, 2)})
	settle()
	states, err := a.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(16*gib), states["pve1"].CommittedBytes,
		"the reservation must cover the gap between the observed 2 GiB and the promised 16 GiB")

	// Phase 2: the memory override lands. The reservation retires and
	// the guest row alone carries the allocation — counted once, not twice.
	rs.setRows([]map[string]any{nodeRow("pve1", 32*gib, 16), guestRow(10001, "pve1", 16*gib, 8)})
	settle()
	states, err = a.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(16*gib), states["pve1"].CommittedBytes,
		"retiring must not double-count the VM")
}

// TestRetiredReservationDoesNotResurrect guards the reason retirement
// DELETES the reservation instead of merely zeroing its term: once the
// VM is destroyed its observed memory returns to 0, and a merely-zeroed
// reservation would spring back to life and withhold memory forever.
func TestRetiredReservationDoesNotResurrect(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
	a := newAccountant(t, rs, func(o *Options) { o.ReserveBytes = 0 })
	ctx := context.Background()

	res, ok, err := a.Reserve(ctx, "pve1", Shape{MemoryBytes: 16 * gib})
	require.NoError(t, err)
	require.True(t, ok)
	res.Bind(10001)

	// Observed at full size: the reservation retires.
	rs.setRows([]map[string]any{nodeRow("pve1", 32*gib, 16), guestRow(10001, "pve1", 16*gib, 8)})
	settle()
	_, err = a.Snapshot(ctx)
	require.NoError(t, err)

	// The VM is destroyed. Its memory must come back in full.
	rs.setRows([]map[string]any{nodeRow("pve1", 32*gib, 16)})
	settle()
	states, err := a.Snapshot(ctx)
	require.NoError(t, err)
	require.Zero(t, states["pve1"].CommittedBytes,
		"a retired reservation must not resurrect when its VM goes away")
}

// TestReservationTTLSweepsLeaks: a clone that hangs past the pool's own
// guards must not withhold memory forever.
func TestReservationTTLSweepsLeaks(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 16*gib, 8)})
	a := newAccountant(t, rs, func(o *Options) {
		o.ReserveBytes = 0
		o.ReservationTTL = 20 * time.Millisecond
	})
	ctx := context.Background()

	_, ok, err := a.Reserve(ctx, "pve1", Shape{MemoryBytes: 16 * gib})
	require.NoError(t, err)
	require.True(t, ok)

	time.Sleep(40 * time.Millisecond)
	states, err := a.Snapshot(ctx)
	require.NoError(t, err)
	require.Zero(t, states["pve1"].CommittedBytes, "an expired reservation must be swept")
}

// TestReserveFreeingCreditsDoomedVMs is the eviction path's arithmetic:
// the victim is still in the snapshot (its qmdestroy has not landed) but
// its memory is already promised to the evicting profile.
func TestReserveFreeingCreditsDoomedVMs(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{
		nodeRow("pve1", 32*gib, 16),
		guestRow(10001, "pve1", 16*gib, 8), // the idle victim
		guestRow(10002, "pve1", 8*gib, 4),
	})
	a := newAccountant(t, rs, func(o *Options) { o.ReserveBytes = 0 })
	ctx := context.Background()

	// 8 GiB free; a 16 GiB clone cannot fit...
	_, ok, err := a.Reserve(ctx, "pve1", Shape{MemoryBytes: 16 * gib})
	require.NoError(t, err)
	require.False(t, ok)

	// ...until the victim's 16 GiB is credited.
	res, ok, err := a.ReserveFreeing(ctx, "pve1", Shape{MemoryBytes: 16 * gib}, []int{10001})
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, res)

	// The claim is exclusive: the victim's memory is spoken for, so a
	// second 16 GiB clone cannot ride along on the same eviction. (The
	// 8 GiB that was already free stays genuinely free — eviction
	// reserves what it frees, it doesn't fence off the whole node.)
	_, ok, err = a.Reserve(ctx, "pve1", Shape{MemoryBytes: 16 * gib})
	require.NoError(t, err)
	require.False(t, ok, "the freed capacity belongs to the evicting profile alone")

	// Once the destroy lands, the credit drops out rather than being
	// counted a second time on top of the now-smaller snapshot.
	rs.setRows([]map[string]any{nodeRow("pve1", 32*gib, 16), guestRow(10002, "pve1", 8*gib, 4)})
	settle()
	states, err := a.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(24*gib), states["pve1"].CommittedBytes,
		"8 GiB of surviving guest + the 16 GiB reservation, counted exactly once")
}

// TestCPUGatedOnlyWhenOvercommitSet: memory is a hard gate, CPU is
// permissive by default because it is time-shared.
func TestCPUGatedOnlyWhenOvercommitSet(t *testing.T) {
	t.Parallel()
	rows := []map[string]any{nodeRow("pve1", 64*gib, 4)}
	ctx := context.Background()

	rs := newResourceServer(t, rows)
	ungated := newAccountant(t, rs, func(o *Options) { o.ReserveBytes = 0 })
	_, ok, err := ungated.Reserve(ctx, "pve1", Shape{MemoryBytes: gib, VCPUs: 64})
	require.NoError(t, err)
	require.True(t, ok, "with cpu_overcommit_ratio unset, vCPUs are never a reason to refuse")

	states, err := ungated.Snapshot(ctx)
	require.NoError(t, err)
	require.False(t, states["pve1"].CPUGated,
		"consumers must be able to tell that the vCPU fields are inert")
	require.Zero(t, states["pve1"].LimitVCPU)

	gated := newAccountant(t, rs, func(o *Options) {
		o.ReserveBytes = 0
		o.CPUOvercommit = 2 // 4 physical cores -> 8 vCPU ceiling
	})
	_, ok, err = gated.Reserve(ctx, "pve1", Shape{MemoryBytes: gib, VCPUs: 8})
	require.NoError(t, err)
	require.True(t, ok, "8 vCPU is exactly the ceiling")
	_, ok, err = gated.Reserve(ctx, "pve1", Shape{MemoryBytes: gib, VCPUs: 1})
	require.NoError(t, err)
	require.False(t, ok, "the 9th vCPU breaches 4 cores x 2.0")

	// The reported ceiling must be the one admission actually applies —
	// cores x ratio, not the raw core count — because the pool's
	// eviction planner sizes a vCPU gap against it.
	states, err = gated.Snapshot(ctx)
	require.NoError(t, err)
	st := states["pve1"]
	require.True(t, st.CPUGated)
	require.Equal(t, 8, st.LimitVCPU, "4 physical cores x 2.0")
	require.Equal(t, 8, st.CommittedVCPU)
	require.Zero(t, st.AvailableVCPU)
}

// TestUnknownNodeNeverFits: an offline or undeclared node is capacity we
// cannot account for, so we refuse to place onto it.
func TestUnknownNodeNeverFits(t *testing.T) {
	t.Parallel()
	offline := nodeRow("pve2", 64*gib, 16)
	offline["status"] = "offline"
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16), offline})
	a := newAccountant(t, rs, func(o *Options) { o.ReserveBytes = 0 })

	fits, err := a.Fits(context.Background(), Shape{MemoryBytes: gib}, []string{"pve1", "pve2", "pve3"})
	require.NoError(t, err)
	require.True(t, fits["pve1"])
	require.False(t, fits["pve2"], "an offline node must never be selected")
	require.False(t, fits["pve3"], "a node the cluster doesn't report must never be selected")
}

// TestNodesOptionRestrictsAccounting: an operator-declared node list
// keeps the accountant out of nodes it was told not to manage.
func TestNodesOptionRestrictsAccounting(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{
		nodeRow("pve1", 32*gib, 16),
		nodeRow("pve2", 32*gib, 16),
	})
	a := newAccountant(t, rs, func(o *Options) { o.Nodes = []string{"pve1"} })

	states, err := a.Snapshot(context.Background())
	require.NoError(t, err)
	require.Contains(t, states, "pve1")
	require.NotContains(t, states, "pve2",
		"a node outside the operator's declared list must not be accounted for")

	fits, err := a.Fits(context.Background(), Shape{MemoryBytes: gib}, []string{"pve1", "pve2"})
	require.NoError(t, err)
	require.True(t, fits["pve1"])
	require.False(t, fits["pve2"], "...nor placed onto")
}

// TestStaleSnapshotBeatsFailingClosed: after one good read, a Proxmox
// blip must degrade to stale data rather than stalling every clone in
// the fleet. Reservations still stack on top, so drift is conservative.
func TestStaleSnapshotBeatsFailingClosed(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
	a := newAccountant(t, rs, func(o *Options) { o.ReserveBytes = 0 })
	ctx := context.Background()

	_, err := a.Snapshot(ctx)
	require.NoError(t, err)

	rs.setFailing(true)
	settle()
	states, err := a.Snapshot(ctx)
	require.NoError(t, err, "a transient Proxmox error must fall back to the last known-good snapshot")
	require.Equal(t, uint64(32*gib), states["pve1"].TotalBytes)

	_, ok, err := a.Reserve(ctx, "pve1", Shape{MemoryBytes: 8 * gib})
	require.NoError(t, err)
	require.True(t, ok, "admission continues against stale data")
}

// TestColdStartFailsClosed is the other half: with no snapshot ever, we
// have no idea what is allocated, so admitting would be guessing.
func TestColdStartFailsClosed(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, nil)
	rs.setFailing(true)
	a := newAccountant(t, rs, nil)

	_, _, err := a.Reserve(context.Background(), "pve1", Shape{MemoryBytes: gib})
	require.ErrorIs(t, err, ErrNoSnapshot,
		"with no snapshot at all, admission must fail rather than guess")
}

// TestConcurrentReserveNeverDoubleSpends is the no-double-spend
// guarantee: N goroutines racing for a node that fits exactly K clones
// must produce exactly K winners.
func TestConcurrentReserveNeverDoubleSpends(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 32)})
	a := newAccountant(t, rs, func(o *Options) {
		o.ReserveBytes = 0
		o.Refresh = time.Minute // pin one snapshot for the whole race
	})
	ctx := context.Background()

	const shapeGiB = 4
	var granted atomic.Int64
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := a.Reserve(ctx, "pve1", Shape{MemoryBytes: shapeGiB * gib})
			require.NoError(t, err)
			if ok {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int64(32/shapeGiB), granted.Load(),
		"exactly as many reservations as the node has room for, no more")
}

// TestSingleflightCollapsesConcurrentFetches keeps a burst of clone
// dispatches from stampeding the Proxmox API on a cold cache.
func TestSingleflightCollapsesConcurrentFetches(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
	a := newAccountant(t, rs, func(o *Options) { o.Refresh = time.Minute })

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.Snapshot(context.Background())
			require.NoError(t, err)
		}()
	}
	wg.Wait()
	require.LessOrEqual(t, rs.calls.Load(), int64(2),
		"concurrent cache misses must collapse into a single API call")
}

// TestHostModeAdmitsAgainstFreeNotGuestSum is the premise of
// host_available: admission plans against the node's actually-free RAM,
// which already folds in the running guests AND host overhead — not the
// sum of the guests' configured maxmem. The same node answers differently
// under the two readings, and admission must follow the free number.
func TestHostModeAdmitsAgainstFreeNotGuestSum(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{
		nodeRow("pve1", 32*gib, 16),
		guestRow(101, "pve1", 8*gib, 4), // a foreign running guest
	})
	// The host has consumed the guest's 8 GiB plus 4 GiB of PVE/ZFS/qemu
	// overhead, leaving 20 GiB genuinely free.
	rs.setNodeStatus("pve1", 20*gib, 32*gib)
	a := newAccountant(t, rs, func(o *Options) { o.MemoryMode = MemoryModeHostAvailable })

	states, err := a.Snapshot(context.Background())
	require.NoError(t, err)
	st := states["pve1"]
	require.Equal(t, uint64(20*gib), st.AvailableBytes,
		"admission follows the free reading, not total minus the guest sum (which would be 24)")
	require.Equal(t, uint64(12*gib), st.CommittedBytes,
		"committed is total minus free — everything the host is actually using")
	require.False(t, st.MemoryPressured)

	ctx := context.Background()
	// 24 GiB is the number the guest-sum arithmetic would admit (32 - 8);
	// the free reading must refuse it.
	_, ok, err := a.Reserve(ctx, "pve1", Shape{MemoryBytes: 24 * gib})
	require.NoError(t, err)
	require.False(t, ok, "admission follows the free reading, not total minus the guest sum")
	// The full free RAM still fits.
	_, ok, err = a.Reserve(ctx, "pve1", Shape{MemoryBytes: 20 * gib})
	require.NoError(t, err)
	require.True(t, ok, "the 20 GiB free reading must be spendable")
}

// TestHostModeOvercommitAdmitsMore: memory_overcommit < 1.0 lets a
// reservation withhold less than its full shape, so the box may be
// oversubscribed; at the 1.0 default it may not.
func TestHostModeOvercommitAdmitsMore(t *testing.T) {
	t.Parallel()
	newRS := func() *resourceServer {
		rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
		rs.setNodeStatus("pve1", 32*gib, 32*gib) // empty node, 32 GiB free
		return rs
	}
	ctx := context.Background()

	// Default overcommit (1.0): a 16 GiB reservation withholds all 16.
	full := newAccountant(t, newRS(), func(o *Options) { o.MemoryMode = MemoryModeHostAvailable })
	_, ok, err := full.Reserve(ctx, "pve1", Shape{MemoryBytes: 16 * gib})
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = full.Reserve(ctx, "pve1", Shape{MemoryBytes: 17 * gib})
	require.NoError(t, err)
	require.False(t, ok, "at overcommit 1.0 the full 16 GiB is withheld, leaving 16")

	// Overcommit 0.5: the same 16 GiB reservation withholds only 8, so the
	// node still has 24 GiB to hand out.
	half := newAccountant(t, newRS(), func(o *Options) {
		o.MemoryMode = MemoryModeHostAvailable
		o.MemoryOvercommit = 0.5
	})
	_, ok, err = half.Reserve(ctx, "pve1", Shape{MemoryBytes: 16 * gib})
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = half.Reserve(ctx, "pve1", Shape{MemoryBytes: 24 * gib})
	require.NoError(t, err)
	require.True(t, ok, "overcommit 0.5 withholds 8 GiB, leaving 24 for a second clone")
}

// TestHostModeMissingStatusFailsClosed: a node whose /status read fails
// has no usable free-RAM number, and memory cannot be guessed, so the
// node must withhold admission entirely rather than plan on a spurious
// zero.
func TestHostModeMissingStatusFailsClosed(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
	// pve1 has no /status payload: the read errors.
	a := newAccountant(t, rs, func(o *Options) { o.MemoryMode = MemoryModeHostAvailable })

	states, err := a.Snapshot(context.Background())
	require.NoError(t, err)
	st := states["pve1"]
	require.True(t, st.MemoryPressured, "a missing free-RAM reading must be surfaced as pressure")
	require.Zero(t, st.AvailableBytes, "admission must fail closed, not guess")

	_, ok, err := a.Reserve(context.Background(), "pve1", Shape{MemoryBytes: gib})
	require.NoError(t, err)
	require.False(t, ok, "a pressured node admits nothing")
}

// TestHostModeStaleStatusFailsClosed: a free-RAM reading older than twice
// the refresh interval is too old to trust, so the node fails closed until
// a fresh read lands. The clock is advanced without sleeping so the cached
// snapshot is not re-fetched (which would make it fresh again).
func TestHostModeStaleStatusFailsClosed(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
	rs.setNodeStatus("pve1", 32*gib, 32*gib)
	a := newAccountant(t, rs, func(o *Options) {
		o.MemoryMode = MemoryModeHostAvailable
		o.Refresh = time.Minute // keep the cache pinned so no re-fetch happens
	})
	ctx := context.Background()

	states, err := a.Snapshot(ctx)
	require.NoError(t, err)
	require.False(t, states["pve1"].MemoryPressured)
	require.Equal(t, uint64(32*gib), states["pve1"].AvailableBytes)

	// Age the snapshot beyond 2*Refresh on the injected clock. The 1-minute
	// cache TTL is not elapsed (no sleep), so resources() still serves this
	// same snapshot; only the staleness check sees the advanced clock.
	a.mu.Lock()
	takenAt := a.lastFresh.takenAt
	a.now = func() time.Time { return takenAt.Add(3 * time.Minute) }
	a.mu.Unlock()

	states, err = a.Snapshot(ctx)
	require.NoError(t, err)
	require.True(t, states["pve1"].MemoryPressured, "a stale reading must not be admitted against")
	require.Zero(t, states["pve1"].AvailableBytes)
}

// TestHostModeReservationRetiresOnObservation: in host mode a
// reservation withholds its RAM from the free pool until the guest's RAM
// shows up in the free reading; once the guest is observed at full size
// the reservation retires and the free reading alone carries it, with no
// double count.
func TestHostModeReservationRetiresOnObservation(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
	rs.setNodeStatus("pve1", 32*gib, 32*gib) // empty node
	a := newAccountant(t, rs, func(o *Options) {
		o.MemoryMode = MemoryModeHostAvailable
		o.ReserveBytes = 0
	})
	ctx := context.Background()

	res, ok, err := a.Reserve(ctx, "pve1", Shape{MemoryBytes: 16 * gib})
	require.NoError(t, err)
	require.True(t, ok)
	res.Bind(10001)
	states, err := a.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(16*gib), states["pve1"].AvailableBytes,
		"the reservation withholds its 16 GiB from the 32 GiB free pool")

	// The clone lands at full size and starts consuming RAM: free drops to
	// 16 GiB. The reservation retires (observed >= shape) and the free
	// reading carries the allocation — available stays 16, not 0.
	rs.setRows([]map[string]any{nodeRow("pve1", 32*gib, 16), guestRow(10001, "pve1", 16*gib, 8)})
	rs.setNodeStatus("pve1", 16*gib, 32*gib)
	settle()
	states, err = a.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(16*gib), states["pve1"].CommittedBytes, "total minus free")
	require.Equal(t, uint64(16*gib), states["pve1"].AvailableBytes,
		"retiring must not subtract the guest a second time off the free reading")
}

// TestAllocatedModeDoesNotReadNodeStatus is the back-compat contract: the
// default allocated mode adds zero Proxmox calls — it never touches
// /nodes/{node}/status, so a fleet that has not opted into host_available
// pays no extra API cost.
func TestAllocatedModeDoesNotReadNodeStatus(t *testing.T) {
	t.Parallel()
	rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 32*gib, 16)})
	a := newAccountant(t, rs, func(o *Options) { o.ReserveBytes = 0 })

	states, err := a.Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(32*gib), states["pve1"].AvailableBytes)
	require.Equal(t, int64(1), rs.calls.Load(),
		"allocated mode reads /cluster/resources only — one call, no /status")
}

// Proxmox reports unused RAM separately from memory available after reclaiming
// cache. Admission must use the latter without losing reservation accounting.
func TestHostModeAvailableMemoryFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		memory    map[string]any
		available uint64
		pressured bool
	}{
		{"available preferred", map[string]any{"total": 64 * gib, "used": 24 * gib, "free": 2 * gib, "available": 40 * gib}, 36 * gib, false},
		{"older API total minus used", map[string]any{"total": 64 * gib, "used": 24 * gib, "free": 2 * gib}, 36 * gib, false},
		{"explicit zero available", map[string]any{"total": 64 * gib, "used": 24 * gib, "available": 0}, 0, false},
		{"missing usage", map[string]any{"total": 64 * gib, "free": 2 * gib}, 0, true},
		{"missing total", map[string]any{"available": 40 * gib}, 0, true},
		{"used exceeds total", map[string]any{"total": 64 * gib, "used": 65 * gib}, 0, true},
		{"available exceeds total", map[string]any{"total": 64 * gib, "available": 65 * gib}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rs := newResourceServer(t, []map[string]any{nodeRow("pve1", 64*gib, 16)})
			rs.nodeStatus = map[string]map[string]any{"pve1": {"memory": tc.memory}}
			a := newAccountant(t, rs, func(o *Options) { o.MemoryMode = MemoryModeHostAvailable; o.ReserveBytes = 4 * gib })
			states, err := a.Snapshot(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.available, states["pve1"].AvailableBytes)
			require.Equal(t, tc.pressured, states["pve1"].MemoryPressured)
			if tc.available == 0 {
				_, ok, err := a.Reserve(context.Background(), "pve1", Shape{MemoryBytes: 4 * gib})
				require.NoError(t, err)
				require.False(t, ok)
			}
			if tc.available > 0 {
				_, ok, err := a.Reserve(context.Background(), "pve1", Shape{MemoryBytes: 4 * gib})
				require.NoError(t, err)
				require.True(t, ok, "cache-backed available memory must admit the Windows worker")
				states, err = a.Snapshot(context.Background())
				require.NoError(t, err)
				require.Equal(t, tc.available-4*gib, states["pve1"].AvailableBytes)
			}
		})
	}
}
