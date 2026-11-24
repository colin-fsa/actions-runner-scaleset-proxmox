package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/pool"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/tags"
	"github.com/stretchr/testify/require"
)

const proofToken = "runner@pve!production"

type recoveryFixture struct {
	advance          int64
	active           []restartTask
	tail             []restartTask
	archive          []restartTask
	guests           []restartGuest
	lxcGuests        []restartGuest
	memory           map[string]any
	taskError        bool
	cloneLogs        map[string]string
	cloneExit        string
	guestStatusError bool
}

func receipt(owner, kind, id string, start, end int64) restartTask {
	return restartTask{fmt.Sprintf("UPID:pve:00000001:00000002:%08X:%s:%s:%s:", start, kind, id, owner), "pve", kind, id, start, end, "OK"}
}

func (f *recoveryFixture) Get(_ context.Context, path string, dst any) error {
	v, err := url.Parse(path)
	if err != nil {
		return err
	}
	encode := func(body any) error {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, dst)
	}
	switch v.Path {
	case "/nodes":
		return encode([]map[string]string{{"node": "pve", "status": "online"}})
	case "/nodes/pve/time":
		return encode(map[string]int64{"time": time.Now().Unix()})
	case "/nodes/pve/tasks":
		if f.taskError {
			return errors.New("HTTP 403")
		}
		if v.Query().Get("userfilter") != "runner@pve" {
			return errors.New("incorrect PVE server filter")
		}
		if v.Query().Get("source") == "active" {
			return encode(f.active)
		}
		if v.Query().Has("until") {
			return encode(f.tail)
		}
		return encode(f.archive)
	case "/nodes/pve/qemu":
		rows := append([]restartGuest{}, f.guests...)
		for i := range rows {
			if rows[i].Uptime > 0 {
				rows[i].Uptime += f.advance
			}
		}
		return encode(rows)
	case "/nodes/pve/qemu/10001/status/current":
		if len(f.guests) != 1 || f.guestStatusError {
			return errors.New("direct guest unavailable")
		}
		g := f.guests[0]
		if g.Status == "running" {
			g.Uptime += f.advance
			g.PID = 1234
		}
		return encode(struct {
			restartGuest
			HA map[string]int `json:"ha"`
		}{g, map[string]int{"managed": 0}})
	case "/nodes/pve/lxc":
		return encode(append([]restartGuest{}, f.lxcGuests...))
	case "/nodes/pve/lxc/11001/status/current":
		if len(f.lxcGuests) != 1 {
			return errors.New("direct LXC unavailable")
		}
		return encode(struct {
			restartGuest
			HA map[string]int `json:"ha"`
		}{f.lxcGuests[0], map[string]int{"managed": 0}})
	case "/nodes/pve/status":
		mem := f.memory
		if mem == nil {
			mem = map[string]any{"total": 1024, "available": 512}
		}
		f.advance += 20
		return encode(map[string]any{"uptime": 1000 + f.advance, "memory": mem})
	default:
		for _, task := range f.archive {
			if v.Path == "/nodes/pve/tasks/"+task.UPID+"/log" && v.Query().Get("start") == "0" && v.Query().Get("limit") == "8" {
				return encode([]map[string]any{{"n": 1, "t": f.cloneLogs[task.UPID]}})
			}
			if v.Path == "/nodes/pve/tasks/"+task.UPID+"/status" {
				exit := f.cloneExit
				if exit == "" {
					exit = "OK"
				}
				return encode(map[string]any{"upid": task.UPID, "node": task.Node, "type": task.Type, "id": task.ID, "starttime": task.Start, "status": "stopped", "exitstatus": exit})
			}
		}
		for _, anchor := range f.tail {
			if v.Path == "/nodes/pve/tasks/"+anchor.UPID+"/status" {
				return encode(map[string]any{"upid": anchor.UPID, "node": anchor.Node, "type": anchor.Type,
					"id": anchor.ID, "starttime": anchor.Start, "status": "stopped", "exitstatus": "OK"})
			}
		}
		return fmt.Errorf("undeclared route %s", path)
	}
}

func TestRestartProofNeedsPriorExactOwnerAndNoUnarchivedWork(t *testing.T) {
	ctx := t.Context()
	cutoff := time.Now().Add(-time.Minute).Unix()
	f := &recoveryFixture{active: []restartTask{}, tail: []restartTask{}, archive: []restartTask{}}
	_, err := readRestartTasks(ctx, f, "pve", proofToken, cutoff)
	require.ErrorContains(t, err, "no prior successful exact-controller task")
	f.tail = []restartTask{receipt("runner@pve!sibling", "qmstart", "10001", cutoff-30, cutoff-20)}
	_, err = readRestartTasks(ctx, f, "pve", proofToken, cutoff)
	require.ErrorContains(t, err, "no prior successful exact-controller task")
	f.tail = append(f.tail, receipt(proofToken, "qmstart", "10001", cutoff-30, cutoff-20))
	_, err = readRestartTasks(ctx, f, "pve", proofToken, cutoff)
	require.NoError(t, err)
	f.active = []restartTask{receipt(proofToken, "qmclone", "9000", cutoff+1, 0)}
	_, err = readRestartTasks(ctx, f, "pve", proofToken, cutoff)
	require.ErrorContains(t, err, "not durably archived")
	f.active = []restartTask{}
	f.taskError = true
	_, err = readRestartTasks(ctx, f, "pve", proofToken, cutoff)
	require.ErrorContains(t, err, "unavailable")
}

func TestRestartProofWaitsForExactAdoptionReadinessAndFreshHost(t *testing.T) {
	ctx := t.Context()
	cutoff := time.Now().Add(-time.Minute).Unix()
	cfg := &config.Config{}
	cfg.Nodes.SingleNode = "pve"
	cfg.Proxmox.Auth.TokenID = proofToken
	cfg.Proxmox.VMIDRange = config.VMIDRange{Min: 10001, Max: 10009}
	cfg.Scalesets = []config.ScaleSetEntry{{Name: "win", Profiles: []config.ProfileConfig{{Name: "default"}}}}
	owner, err := tags.OwnerTag("win")
	require.NoError(t, err)
	wireTags := strings.Join([]string{tags.Marker, owner, tags.ProfileTag("default")}, ";")
	f := &recoveryFixture{
		active: []restartTask{}, tail: []restartTask{receipt(proofToken, "qmclone", "9000", cutoff-40, cutoff-20)},
		archive: []restartTask{receipt(proofToken, "qmstart", "10001", cutoff+2, cutoff+5)},
		guests:  []restartGuest{{ID: 10001, Name: "gh-runner-win-10001", Tags: wireTags, Status: "running", Uptime: 15}},
	}
	row := pool.RowSnapshot{VMID: 10001, Node: "pve", Profile: "default", Name: "gh-runner-win-10001", State: "hot", CreatedAt: time.Now()}
	observed := map[string][]pool.RowSnapshot{"win": {row}}
	ready := func(_ context.Context, v *provisioner.VM) error {
		if v.VMID != row.VMID || !v.CreatedAt.Equal(row.CreatedAt) {
			return errors.New("wrong generation")
		}
		return nil
	}
	require.NoError(t, proveScopedRestart(ctx, f, cfg, observed, ready, cutoff, 0, &restartResult{}))
	var reobservation restartResult
	err = proveScopedRestart(ctx, f, cfg, nil, ready, cutoff, 0, &reobservation)
	require.ErrorContains(t, err, "not represented by adoption")
	require.Len(t, reobservation.late, 1)
	require.Equal(t, "win", reobservation.late[0].manager)
	require.Equal(t, row.Name, reobservation.late[0].vm.Name)
	require.Empty(t, reobservation.holds, "one observation never opens admission")
	err = proveScopedRestart(ctx, f, cfg, observed, func(context.Context, *provisioner.VM) error { return errors.New("guest still booting") }, cutoff, 0, &restartResult{})
	require.ErrorContains(t, err, "readiness and configured footprint are unproven")
	f.memory = map[string]any{"total": int64(168563757056), "used": int64(96489193472), "free": int64(47680757760), "available": nil}
	require.NoError(t, proveScopedRestart(ctx, f, cfg, observed, ready, cutoff, 0, &restartResult{}), "older API uses validated total-minus-used, not free")
	f.memory["available"] = int64(168563757057)
	err = proveScopedRestart(ctx, f, cfg, observed, ready, cutoff, 0, &restartResult{})
	require.ErrorContains(t, err, "available memory unavailable", "invalid explicit field cannot fall back to used")
	delete(f.memory, "available")
	delete(f.memory, "used")
	err = proveScopedRestart(ctx, f, cfg, observed, ready, cutoff, 0, &restartResult{})
	require.ErrorContains(t, err, "available memory unavailable", "free alone is not valid")
	f.memory = nil
	f.guests[0].MaxMemory, f.guests[0].CPUs = 4*1024*1024*1024, 4
	row.State = "assigned" // WinRE / unreadable handoff: unassignable, not proof of no job.
	observed["win"] = []pool.RowSnapshot{row}
	var degraded restartResult
	require.NoError(t, proveScopedRestart(ctx, f, cfg, observed, func(context.Context, *provisioner.VM) error {
		return errors.New("QGA unavailable in WinRE")
	}, cutoff, 0, &degraded))
	require.Len(t, degraded.holds, 1)
	require.Equal(t, uint64(4*1024*1024*1024), degraded.holds[0].Shape.MemoryBytes)
	require.Equal(t, row.CreatedAt, degraded.holds[0].Generation)
	require.Empty(t, degraded.stopped, "uncertain running guest is not cleaned up")
	f.guests[0].Status, f.guests[0].Uptime = "stopped", 0
	row.State, row.RunnerID, row.AdoptionProtected = "running", 33, true
	observed["win"] = []pool.RowSnapshot{row}
	require.NoError(t, proveScopedRestart(ctx, f, cfg, observed, func(context.Context, *provisioner.VM) error {
		return errors.New("do not ping a stopped guest")
	}, cutoff, 0, &restartResult{}), "an existing job may finish while proof waits; direct stopped reads suffice without reclassifying it")
	row.State, row.RestartCleanupPending, row.RunnerID = "assigned", true, 0
	observed["win"] = []pool.RowSnapshot{row}
	var stopped restartResult
	require.NoError(t, proveScopedRestart(ctx, f, cfg, observed, ready, cutoff, 0, &stopped))
	require.Len(t, stopped.stopped, 1)
	require.Equal(t, row.CreatedAt, stopped.stopped[0].row.CreatedAt)
}

func TestRestartPretagUsesExactOwnCloneDestinationAndTerminal(t *testing.T) {
	for _, kind := range []string{"qemu", "lxc"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().Unix()
			action, subject := "qmclone", "VM"
			if kind == "lxc" {
				action, subject = "vzclone", "container"
			}
			cfg := &config.Config{}
			cfg.Nodes.SingleNode = "pve"
			cfg.Proxmox.Auth.TokenID = proofToken
			set := config.ScaleSetEntry{Name: "runner", Profiles: []config.ProfileConfig{{Name: "default", GuestType: kind, TemplateVMID: 9000}}}
			task := receipt(proofToken, action, "9000", now-10, now-8)
			correct := fmt.Sprintf("creating a clone of %s 9000 with ID 10001", subject)
			f := &recoveryFixture{archive: []restartTask{task}, cloneLogs: map[string]string{task.UPID: correct}}
			input := map[string]restartTask{task.UPID: task}
			profile, upid, err := pretagProfile(t.Context(), f, cfg, set, kind, 10001, input)
			require.NoError(t, err)
			require.Equal(t, "default", profile)
			require.Equal(t, task.UPID, upid)
			for _, bad := range []string{"prefix " + correct, correct + "0", strings.Replace(correct, "9000", "90000", 1), strings.Replace(correct, "10001", "010001", 1), correct + " suffix"} {
				f.cloneLogs[task.UPID] = bad
				_, _, err = pretagProfile(t.Context(), f, cfg, set, kind, 10001, input)
				require.ErrorContains(t, err, "no exact")
			}
			f.cloneLogs[task.UPID] = correct
			f.cloneExit = "TASK ERROR"
			_, _, err = pretagProfile(t.Context(), f, cfg, set, kind, 10001, input)
			require.ErrorContains(t, err, "terminal success")
			sibling := receipt("runner@pve!sibling", action, "9000", now-9, now-8)
			_, _, err = pretagProfile(t.Context(), f, cfg, set, kind, 10001, map[string]restartTask{sibling.UPID: sibling})
			require.ErrorContains(t, err, "no exact")
		})
	}
}

func TestRestartPretagQuarantineOnlyAfterIndependentStableReads(t *testing.T) {
	ctx := t.Context()
	cutoff := time.Now().Add(-time.Minute).Unix()
	cfg := &config.Config{}
	cfg.Nodes.SingleNode = "pve"
	cfg.Proxmox.Auth.TokenID = proofToken
	cfg.Proxmox.VMIDRange = config.VMIDRange{Min: 10001, Max: 10009}
	cfg.Scalesets = []config.ScaleSetEntry{{Name: "win", Profiles: []config.ProfileConfig{{Name: "default", TemplateVMID: 9000}}}}
	task := receipt(proofToken, "qmclone", "9000", cutoff+1, cutoff+8)
	f := &recoveryFixture{active: []restartTask{}, tail: []restartTask{receipt(proofToken, "qmstart", "123", cutoff-40, cutoff-30)},
		archive: []restartTask{task}, cloneLogs: map[string]string{task.UPID: "creating a clone of VM 9000 with ID 10001"},
		guests: []restartGuest{{ID: 10001, Name: "gh-runner-win-10001", Status: "stopped"}},
	}
	ready := func(context.Context, *provisioner.VM) error {
		return errors.New("must not touch stopped pre-tag guest")
	}
	var result restartResult
	err := proveScopedRestart(ctx, f, cfg, nil, ready, cutoff, 0, &result)
	require.ErrorContains(t, err, "not represented")
	require.Len(t, result.late, 1)
	require.True(t, result.late[0].pretag)
	require.Empty(t, result.stopped)
	require.Equal(t, task.UPID, result.late[0].receipt)
	for i := 0; i < 3; i++ {
		// New leader has no memory of the previous exclusion. Case 1
		// represents a long clone initiated before the window; case 2
		// represents a later restart after task history was rotated.
		if i == 1 {
			oldClone := receipt(proofToken, "qmclone", "9000", cutoff-120, cutoff+8)
			f.tail = append(f.tail, oldClone)
			f.archive = []restartTask{}
		}
		if i == 2 {
			f.tail = f.tail[:1]
		}
		var reobserve restartResult
		require.ErrorContains(t, proveScopedRestart(ctx, f, cfg, nil, ready, cutoff, 0, &reobserve), "not represented")
		require.Len(t, reobserve.late, 1)
		require.True(t, reobserve.late[0].pretag)
		if i > 0 {
			require.Empty(t, reobserve.late[0].receipt)
		}
		rows := map[string][]pool.RowSnapshot{"win": {{VMID: 10001, Name: "gh-runner-win-10001", Node: "pve", Profile: "default", CreatedAt: time.Now(), State: "excluded", AdoptionProtected: true, RestartPretagQuarantine: true}}}
		var recovered restartResult
		require.NoError(t, proveScopedRestart(ctx, f, cfg, rows, ready, cutoff, 0, &recovered))
		require.Empty(t, recovered.holds)
		require.Empty(t, recovered.stopped)
	}
	f.guests[0].Status, f.guests[0].Uptime = "running", 12
	require.ErrorContains(t, proveScopedRestart(ctx, f, cfg, nil, ready, cutoff, 0, &restartResult{}), "not a clean stopped pre-tag")
	f.guests[0].Status, f.guests[0].Uptime = "stopped", 0
	f.guestStatusError = true
	require.ErrorContains(t, proveScopedRestart(ctx, f, cfg, nil, ready, cutoff, 0, &restartResult{}), "direct status unavailable")
	f.guestStatusError = false
	f.active = []restartTask{receipt(proofToken, "qmclone", "9000", cutoff+1, 0)}
	require.ErrorContains(t, proveScopedRestart(ctx, f, cfg, nil, ready, cutoff, 0, &restartResult{}), "active or not durably archived")
}
