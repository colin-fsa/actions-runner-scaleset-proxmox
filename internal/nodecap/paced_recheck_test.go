package nodecap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
)

func TestPacedReceiptLossOrNoTaskFreesSlotsNotMemoryAfterTwoObservations(t *testing.T) {
	t.Run("lost_receipt", func(t *testing.T) { exerciseInactiveReceiptRecovery(t, false) })
	t.Run("ready_but_normal_boot_identity_never_visible", func(t *testing.T) { exerciseInactiveReceiptRecovery(t, true) })
}

func exerciseInactiveReceiptRecovery(t *testing.T, ready bool) {
	t.Helper()
	const owner = "runner@pve!production"
	var mu sync.Mutex
	now := time.Now().Truncate(time.Second)
	begun := now
	active := false
	lost := int64(10001)
	mkTask := func(id int64, at time.Time) map[string]any {
		epoch := at.Unix()
		return map[string]any{
			"upid": fmt.Sprintf("UPID:pve1:00000001:00000002:%08X:qmstart:%d:%s:", epoch, id, owner),
			"node": "pve1", "type": "qmstart", "id": strconv.FormatInt(id, 10),
			"starttime": epoch, "endtime": epoch + 1, "status": "OK",
		}
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		write := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": data}) }
		switch {
		case r.URL.Path == "/cluster/resources":
			write([]map[string]any{{"type": "node", "node": "pve1", "status": "online", "maxmem": 64 * gib, "maxcpu": 32}})
		case r.URL.Path == "/nodes/pve1/status":
			write(map[string]any{"memory": map[string]any{"total": 64 * gib, "available": 60 * gib}})
		case r.URL.Path == "/nodes/pve1/tasks":
			if r.URL.Query().Get("userfilter") != "runner@pve" {
				http.Error(w, "bad owner filter", 400)
				return
			}
			switch r.URL.Query().Get("source") {
			case "active":
				if active {
					write([]map[string]any{mkTask(lost, begun)})
					return
				}
				write([]map[string]any{})
			case "archive":
				vmid := r.URL.Query().Get("vmid")
				if vmid == "" {
					write([]map[string]any{mkTask(9999, begun.Add(-time.Minute))})
					return
				}
				if vmid == strconv.FormatInt(lost, 10) {
					write([]map[string]any{mkTask(lost, begun)})
					return
				}
				write([]map[string]any{}) // AlreadyRunning/no submitted task.
			}
		case strings.Contains(r.URL.Path, "/tasks/"):
			task := mkTask(lost, begun)
			if strings.Contains(r.URL.Path, ":9999:") {
				task = mkTask(9999, begun.Add(-time.Minute))
			}
			task["status"], task["exitstatus"] = "stopped", "OK"
			write(task)
		case strings.HasSuffix(r.URL.Path, "/status/current"):
			fields := strings.Split(r.URL.Path, "/")
			id, _ := strconv.Atoi(fields[4])
			write(map[string]any{"vmid": id, "name": fmt.Sprintf("runner-%d", id), "status": "running", "pid": id + 1000,
				"uptime": int64(now.Sub(begun)/time.Second) + 10, "ha": map[string]int{"managed": 0}})
		default:
			http.Error(w, "unexpected route", 400)
		}
	}))
	defer s.Close()
	a, err := New(proxmox.NewClient(s.URL), Options{MemoryMode: MemoryModeHostAvailable, ReserveBytes: 4 * gib,
		Refresh: time.Minute, PacedStartLimit: 2, PacedStartInterval: 15 * time.Second, PacedTaskOwner: owner, Log: quietLogger()})
	require.NoError(t, err)
	a.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); defer mu.Unlock(); now = now.Add(d) }
	setActive := func(v bool) { mu.Lock(); defer mu.Unlock(); active = v }
	shape := Shape{MemoryBytes: 10 * gib}
	receipts := make([]func(), 0, 2)
	for _, id := range []int{10001, 10002} {
		r, ok, err := a.ReserveExistingForPacedStart(t.Context(), "pve1", shape, id, begun, false)
		require.NoError(t, err)
		require.True(t, ok)
		require.True(t, r.(*reservation).BeginStart())
		ctx := WithPacedTaskReceipt(t.Context(), r)
		ReportPacedSubmissionReturned(ctx, fmt.Sprintf("runner-%d", id), "qemu")
		if ready && int64(id) == lost {
			upid := mkTask(lost, begun)["upid"].(string)
			ReportPacedTask(ctx, "pve1", id, upid, false)
			ReportPacedTask(ctx, "pve1", id, upid, true)
			ReportPacedReady(ctx, fmt.Sprintf("runner-%d", id))
		}
		receipts = append(receipts, func() { ReportPacedWorkerDone(ctx) })
	}
	try := func(id int) bool {
		_, ok, err := a.ReserveExistingForPacedStart(t.Context(), "pve1", shape, id, begun, false)
		require.NoError(t, err)
		return ok
	}
	advance(20 * time.Second)
	require.True(t, try(10003), "workers do not block measured headroom")
	for _, done := range receipts {
		done()
	}
	setActive(true)
	advance(5 * time.Second)
	require.True(t, try(10005), "an active task affects proof, not headroom")
	setActive(false)
	advance(5 * time.Second)
	require.True(t, try(10006), "starts need no readiness slot")
	advance(20 * time.Second)
	require.True(t, try(10007))
	require.True(t, try(10004), "inactive claims never occupy launch slots")
	st, err := a.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, uint64(56*gib), st["pve1"].AvailableBytes, "incomplete claims remain observational, not RAM debits")
}

func TestPacedTerminalBootUsesFreshDirectHostAndNoReadiness(t *testing.T) {
	for _, op := range []string{"qmstart", "vzstart", "vzreboot"} {
		t.Run(op, func(t *testing.T) {
			const owner = "runner@pve!production"
			const id = 10001
			const node = "pve1"
			const name = "runner-10001"
			kind := "qemu"
			if strings.HasPrefix(op, "vz") {
				kind = "lxc"
			}
			var mu sync.Mutex
			now := time.Now().Truncate(time.Second)
			begun := now
			boot := begun.Add(time.Second)
			pid, guestName, status, exit := 9172, name, "running", "OK"
			available := uint64(60 * gib)
			badHost := false
			degraded := false
			upid := fmt.Sprintf("UPID:pve1:00000001:00000002:%08X:%s:%d:%s:", begun.Unix(), op, id, owner)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				write := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": data}) }
				switch {
				case req.URL.Path == "/cluster/resources":
					// Lagging uptime would place the original 3s boot *after*
					// the successful task's completion. It must not gate proof.
					write([]map[string]any{nodeRow(node, 64*gib, 32),
						{"type": kind, "node": node, "vmid": id, "name": name, "status": "running",
							"uptime": max(int64(now.Sub(boot)/time.Second)-8, 0), "maxmem": 10 * gib}})
				case req.URL.Path == "/nodes/pve1/status":
					if badHost {
						http.Error(w, "node status unavailable", http.StatusBadRequest)
						return
					}
					var mem any = available
					if degraded {
						mem = int64(-1)
					}
					write(map[string]any{"memory": map[string]any{"total": 64 * gib, "available": mem}})
				case strings.HasSuffix(req.URL.Path, "/"+upid+"/status"):
					write(map[string]any{"upid": upid, "node": node, "type": op, "id": "10001",
						"starttime": begun.Unix(), "status": "stopped", "exitstatus": exit})
				case req.URL.Path == fmt.Sprintf("/nodes/%s/%s/%d/status/current", node, kind, id):
					reportPID, uptime := pid, int64(now.Sub(boot)/time.Second)
					if status == "stopped" {
						reportPID, uptime = 0, 0
					}
					write(map[string]any{"vmid": "10001", "name": guestName, "status": status, "pid": reportPID,
						"uptime": uptime, "ha": map[string]int{"managed": 0}})
				default:
					http.Error(w, "unexpected route", http.StatusBadRequest)
				}
			}))
			t.Cleanup(srv.Close)
			a, err := New(proxmox.NewClient(srv.URL), Options{MemoryMode: MemoryModeHostAvailable, ReserveBytes: 4 * gib,
				Refresh: time.Minute, PacedStartLimit: 1, PacedStartInterval: 15 * time.Second, PacedTaskOwner: owner, Log: quietLogger()})
			require.NoError(t, err)
			a.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
			advance := func(d time.Duration) { mu.Lock(); defer mu.Unlock(); now = now.Add(d) }
			gen := begun.Add(-time.Second)
			shape := Shape{MemoryBytes: 10 * gib}
			r, ok, err := a.ReserveExistingForPacedStart(t.Context(), node, shape, id, gen, false)
			require.NoError(t, err)
			require.True(t, ok)
			require.True(t, r.(*reservation).BeginStart())
			ctx := WithPacedTaskReceipt(t.Context(), r)
			ReportPacedSubmissionReturned(ctx, name, kind)
			ReportPacedTask(ctx, node, id, upid, false)
			advance(3 * time.Second)
			ReportPacedTask(ctx, node, id, "wrong-receipt", true)
			check := func() NodeState {
				a.cache.Delete(snapshotCacheKey)
				res, err := a.Snapshot(t.Context())
				require.NoError(t, err)
				return res[node]
			}
			advance(3 * time.Second)
			require.Equal(t, uint64(56*gib), check().AvailableBytes, "physical headroom ignores claim RAM")
			require.False(t, r.(*reservation).released, "unconfirmed task holds retains ownership evidence")
			ReportPacedTask(ctx, node, id, upid, true)
			advance(5 * time.Second)
			require.Equal(t, uint64(56*gib), check().AvailableBytes, "physical headroom ignores claim RAM")
			require.False(t, r.(*reservation).released, "one exact sample still holds retains ownership evidence")
			advance(16 * time.Second)
			mu.Lock()
			guestName = "reused-10001"
			mu.Unlock()
			require.Equal(t, uint64(56*gib), check().AvailableBytes, "physical headroom ignores claim RAM")
			require.False(t, r.(*reservation).released, "same ID but wrong name is not proof retains ownership evidence")
			advance(6 * time.Second)
			mu.Lock()
			guestName, exit = name, "failed"
			mu.Unlock()
			require.Equal(t, uint64(56*gib), check().AvailableBytes, "physical headroom ignores claim RAM")
			require.False(t, r.(*reservation).released, "terminal failure is not success retains ownership evidence")
			advance(6 * time.Second)
			mu.Lock()
			exit, status = "OK", "stopped"
			mu.Unlock()
			require.Equal(t, uint64(56*gib), check().AvailableBytes, "physical headroom ignores claim RAM")
			require.False(t, r.(*reservation).released, "stopped guest is not accounted boot retains ownership evidence")
			advance(6 * time.Second)
			mu.Lock()
			status, degraded = "running", true
			mu.Unlock()
			require.Zero(t, check().AvailableBytes, "invalid live host reading fails closed")
			advance(6 * time.Second)
			mu.Lock()
			degraded = false
			mu.Unlock()
			require.Equal(t, uint64(56*gib), check().AvailableBytes, "physical headroom ignores claim RAM")
			require.False(t, r.(*reservation).released, "first reliable post-failure sample retains ownership evidence")
			advance(16 * time.Second)
			mu.Lock()
			pid++
			mu.Unlock()
			require.Equal(t, uint64(56*gib), check().AvailableBytes, "physical headroom ignores claim RAM")
			require.False(t, r.(*reservation).released, "PID change restarts the quiet observation retains ownership evidence")
			advance(16 * time.Second)
			old, err := a.fetch(t.Context())
			require.NoError(t, err)
			advance(time.Second)
			require.Equal(t, uint64(56*gib), check().AvailableBytes, "no guest agent, runner readiness, or worker done needed")
			require.True(t, r.(*reservation).released)
			_, ok, err = a.reserveExistingFrom(old, node, shape, id+1, gen, false, true)
			require.NoError(t, err)
			require.False(t, ok, "an older in-flight snapshot cannot spend newly released credit")
			mu.Lock()
			badHost = true
			mu.Unlock()
			_, ok, err = a.ReserveExistingForPacedStart(t.Context(), node, shape, id+1, gen, false)
			require.NoError(t, err)
			require.False(t, ok, "resolved holds do not bypass fresh host admission")
		})
	}
}
