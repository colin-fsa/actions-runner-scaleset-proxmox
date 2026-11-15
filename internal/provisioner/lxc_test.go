package provisioner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/tags"
)

// lxcReq is one request the fake saw.
type lxcReq struct {
	Method string
	Path   string
	Query  url.Values
	Body   map[string]any
}

// lxcCT is a container held by the fake.
type lxcCT struct {
	Name     string
	Running  bool
	Template bool
	Tags     string
	Config   map[string]any
}

// lxcFake is a small stateful Proxmox fake covering the container
// endpoints the provisioner uses, plus the node / qemu listings and task
// status. Every task completes immediately with exit status OK unless
// failPath matches.
type lxcFake struct {
	t        *testing.T
	mu       sync.Mutex
	node     string
	cts      map[int]*lxcCT
	qemu     string // raw JSON array for GET /nodes/{node}/qemu
	reqs     []lxcReq
	failPath map[string]string // "METHOD path" -> 500 status text
	rules    map[int][]map[string]any
	tasks    int
}

func newLXCFake(t *testing.T) (*lxcFake, *httptest.Server) {
	t.Helper()
	f := &lxcFake{
		t:        t,
		node:     "pve1",
		cts:      map[int]*lxcCT{},
		qemu:     `[]`,
		failPath: map[string]string{},
		rules:    map[int][]map[string]any{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *lxcFake) requests() []lxcReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lxcReq(nil), f.reqs...)
}

// find returns the recorded requests matching method and path.
func (f *lxcFake) find(method, path string) []lxcReq {
	var out []lxcReq
	for _, r := range f.requests() {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *lxcFake) newUPID(kind string, vmid int) string {
	f.tasks++
	return fmt.Sprintf("UPID:%s:%08X:00000001:5F3D0000:%s:%d:scaleset@pve!automation:", f.node, f.tasks, kind, vmid)
}

func writeJSONData(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": v})
}

// badRequest replies the way the provisioner's error classifiers can see
// the message under httptest: real PVE carries it in the HTTP reason
// phrase of a 500, which go-proxmox surfaces, but httptest cannot set a
// custom reason phrase, while go-proxmox includes a 400 body verbatim.
func badRequest(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	b, _ := json.Marshal(map[string]any{"errors": map[string]string{"detail": msg}})
	_, _ = w.Write(b)
}

func (f *lxcFake) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, lxcReq{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: body})
	if msg, ok := f.failPath[r.Method+" "+r.URL.Path]; ok {
		badRequest(w, msg)
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.URL.Path == "/nodes":
		writeJSONData(w, []map[string]any{{"node": f.node, "status": "online"}})
		return
	case len(parts) == 3 && parts[2] == "status":
		writeJSONData(w, map[string]any{})
		return
	case len(parts) == 3 && parts[2] == "qemu":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":`+f.qemu+`}`)
		return
	case len(parts) == 3 && parts[2] == "lxc":
		out := []map[string]any{}
		for id, ct := range f.cts {
			st := "stopped"
			if ct.Running {
				st = "running"
			}
			out = append(out, map[string]any{"vmid": id, "name": ct.Name, "tags": ct.Tags, "status": st})
		}
		writeJSONData(w, out)
		return
	case len(parts) == 5 && parts[2] == "tasks":
		writeJSONData(w, map[string]any{"upid": parts[3], "status": "stopped", "exitstatus": "OK"})
		return
	}
	if len(parts) < 4 || parts[2] != "lxc" {
		http.NotFound(w, r)
		return
	}
	var vmid int
	_, _ = fmt.Sscanf(parts[3], "%d", &vmid)
	ct := f.cts[vmid]
	rest := strings.Join(parts[4:], "/")
	if ct == nil {
		badRequest(w, fmt.Sprintf("Configuration file 'nodes/pve1/lxc/%d.conf' does not exist", vmid))
		return
	}
	switch r.Method + " " + rest {
	case "POST clone":
		newID := int(body["newid"].(float64))
		cfg := map[string]any{}
		for k, v := range ct.Config {
			cfg[k] = v
		}
		delete(cfg, "template")
		f.cts[newID] = &lxcCT{Name: fmt.Sprint(body["hostname"]), Config: cfg}
		writeJSONData(w, f.newUPID("vzclone", vmid))
	case "GET config":
		cfg := map[string]any{}
		for k, v := range ct.Config {
			cfg[k] = v
		}
		if ct.Template {
			cfg["template"] = 1
		}
		writeJSONData(w, cfg)
	case "PUT config":
		for k, v := range body {
			ct.Config[k] = v
			if k == "tags" {
				ct.Tags = fmt.Sprint(v)
			}
		}
		writeJSONData(w, nil)
	case "PUT resize":
		writeJSONData(w, f.newUPID("resize", vmid))
	case "GET status/current":
		st := "stopped"
		if ct.Running {
			st = "running"
		}
		writeJSONData(w, map[string]any{"status": st, "name": ct.Name, "vmid": vmid})
	case "POST status/start":
		if ct.Running {
			badRequest(w, fmt.Sprintf("CT %d already running", vmid))
			return
		}
		ct.Running = true
		writeJSONData(w, f.newUPID("vzstart", vmid))
	case "POST status/reboot":
		ct.Running = true
		writeJSONData(w, f.newUPID("vzreboot", vmid))
	case "POST status/shutdown", "POST status/stop":
		ct.Running = false
		writeJSONData(w, f.newUPID("vzshutdown", vmid))
	case "DELETE ":
		delete(f.cts, vmid)
		writeJSONData(w, f.newUPID("vzdestroy", vmid))
	case "POST firewall/rules":
		f.rules[vmid] = append(f.rules[vmid], body)
		writeJSONData(w, nil)
	case "GET firewall/rules":
		writeJSONData(w, f.rules[vmid])
	case "PUT firewall/options":
		writeJSONData(w, nil)
	default:
		f.t.Errorf("lxc fake: unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

// newLXCProvisioner returns a provisioner whose scale set has one LXC
// profile "ct" (template 9100) and one QEMU profile "vm".
func newLXCProvisioner(t *testing.T, srv *httptest.Server) *pmox {
	t.Helper()
	p := newTestProvisioner(t, srv, "pve1")
	p.jitRestarts = newTracker(time.Minute)
	p.profiles = map[string]config.ProfileConfig{
		"ct": {Name: "ct", GuestType: config.GuestTypeLXC, TemplateVMID: 9100},
		"vm": {Name: "vm"},
	}
	return p
}

func validJIT(t *testing.T) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(`{"runner":"secret-token-value","url":"https://example.invalid"}`))
}

func TestLXCClone_LinkedRoutesToLXCEndpoints(t *testing.T) {
	t.Parallel()
	f, srv := newLXCFake(t)
	f.cts[9100] = &lxcCT{Name: "tmpl", Template: true, Config: map[string]any{
		"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01,ip=dhcp,type=veth",
	}}
	p := newLXCProvisioner(t, srv)
	p.cfg.Firewall = config.FirewallConfig{Enabled: true, SecurityGroup: "runners"}

	vm, err := p.Clone(context.Background(), CloneOptions{
		NewVMID: 10042, Node: "pve1", Name: "gh-runner-test-10042", Linked: true, PoweredOn: true,
		Profile: "ct", TemplateVMID: 9100, CPUCores: 4, MemoryMB: 8192, DiskGB: 30,
	})
	require.NoError(t, err)
	require.Equal(t, &VM{VMID: 10042, Node: "pve1", Name: "gh-runner-test-10042", GuestType: config.GuestTypeLXC}, vm)

	clone := f.find(http.MethodPost, "/nodes/pve1/lxc/9100/clone")
	require.Len(t, clone, 1)
	require.EqualValues(t, 10042, clone[0].Body["newid"])
	require.Equal(t, "gh-runner-test-10042", clone[0].Body["hostname"])
	require.EqualValues(t, 0, clone[0].Body["full"], "linked clone must send full=0 explicitly")
	require.NotContains(t, clone[0].Body, "pool")
	require.NotContains(t, clone[0].Body, "target")
	require.Contains(t, clone[0].Body["description"], "test-scaleset")

	put := f.find(http.MethodPut, "/nodes/pve1/lxc/10042/config")
	require.Len(t, put, 1)
	cfg := put[0].Body
	require.EqualValues(t, 4, cfg["cores"])
	require.EqualValues(t, 8192, cfg["memory"])
	require.EqualValues(t, 0, cfg["swap"])
	require.EqualValues(t, 0, cfg["protection"])
	wantTags, err := tags.Initial("test-scaleset", "ct", "")
	require.NoError(t, err)
	require.Equal(t, tags.Encode(wantTags), cfg["tags"], "container gets the same owner/profile/template tags as a QEMU clone")
	require.True(t, tags.IsOwnedBy(fmt.Sprint(cfg["tags"]), "test-scaleset"))
	require.Equal(t, "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01,ip=dhcp,type=veth,firewall=1", cfg["net0"])

	resize := f.find(http.MethodPut, "/nodes/pve1/lxc/10042/resize")
	require.Len(t, resize, 1)
	require.Equal(t, "rootfs", resize[0].Body["disk"])
	require.Equal(t, "30G", resize[0].Body["size"])

	rules := f.find(http.MethodPost, "/nodes/pve1/lxc/10042/firewall/rules")
	require.Len(t, rules, 1)
	require.Equal(t, "group", rules[0].Body["type"])
	require.Equal(t, "runners", rules[0].Body["action"])
	require.Len(t, f.find(http.MethodPut, "/nodes/pve1/lxc/10042/firewall/options"), 1)

	// Firewall lands before the first start.
	var order []string
	for _, r := range f.requests() {
		if strings.HasPrefix(r.Path, "/nodes/pve1/lxc/10042/") && r.Method != http.MethodGet {
			order = append(order, r.Method+" "+strings.TrimPrefix(r.Path, "/nodes/pve1/lxc/10042/"))
		}
	}
	require.Equal(t, []string{"PUT config", "PUT resize", "POST firewall/rules", "PUT firewall/options", "POST status/start"}, order)

	for _, r := range f.requests() {
		require.NotContains(t, r.Path, "/qemu/", "an LXC clone must never touch qemu endpoints")
	}
}

func TestLXCClone_FullCloneTargetsNodeAndStorage(t *testing.T) {
	t.Parallel()
	f, srv := newLXCFake(t)
	f.cts[9100] = &lxcCT{Name: "tmpl", Template: true, Config: map[string]any{}}
	p := newLXCProvisioner(t, srv)
	params := buildLXCCloneParams("s", CloneOptions{NewVMID: 1, Name: "n", Node: "pve2", Storage: "fast", Profile: "ct"}, "pve1")
	require.EqualValues(t, 1, params["full"])
	require.Equal(t, "pve2", params["target"])
	require.Equal(t, "fast", params["storage"])
	require.NotContains(t, params, "pool")

	// Profile-derived guest type routes to LXC even without opts.GuestType.
	vm, err := p.Clone(context.Background(), CloneOptions{NewVMID: 10043, Node: "pve1", Name: "n", Profile: "ct", TemplateVMID: 9100})
	require.NoError(t, err)
	require.Equal(t, config.GuestTypeLXC, vm.GuestType)
	require.Empty(t, f.find(http.MethodPost, "/nodes/pve1/lxc/10043/status/start"), "PoweredOn=false must not start")
	require.Empty(t, f.find(http.MethodPost, "/nodes/pve1/lxc/10043/firewall/rules"), "firewall disabled: no firewall calls")
}

func TestLXC_StartStopDestroyRouting(t *testing.T) {
	t.Parallel()
	f, srv := newLXCFake(t)
	f.cts[10050] = &lxcCT{Name: "c", Config: map[string]any{}}
	p := newLXCProvisioner(t, srv)
	vm := &VM{VMID: 10050, Node: "pve1", Profile: "ct"}

	require.NoError(t, p.Start(context.Background(), vm))
	require.Len(t, f.find(http.MethodPost, "/nodes/pve1/lxc/10050/status/start"), 1)
	// Already running is success.
	require.NoError(t, p.Start(context.Background(), vm))

	state, err := p.PowerState(context.Background(), vm)
	require.NoError(t, err)
	require.Equal(t, "running", state)
	require.NoError(t, p.WaitReady(context.Background(), vm, time.Second))

	require.NoError(t, p.Stop(context.Background(), vm))
	sd := f.find(http.MethodPost, "/nodes/pve1/lxc/10050/status/shutdown")
	require.Len(t, sd, 1)
	require.EqualValues(t, 1, sd[0].Body["forceStop"])
	// Stopping a stopped container is a no-op.
	require.NoError(t, p.Stop(context.Background(), vm))
	require.Len(t, f.find(http.MethodPost, "/nodes/pve1/lxc/10050/status/shutdown"), 1)

	require.NoError(t, p.Start(context.Background(), vm))
	// Destroy without a profile: the provisioner probes and finds a container.
	require.NoError(t, p.Destroy(context.Background(), &VM{VMID: 10050, Node: "pve1"}))
	del := f.find(http.MethodDelete, "/nodes/pve1/lxc/10050")
	require.Len(t, del, 1)
	require.Equal(t, "1", del[0].Query.Get("purge"))
	require.Equal(t, "1", del[0].Query.Get("force"))
	require.Len(t, f.find(http.MethodPost, "/nodes/pve1/lxc/10050/status/shutdown"), 2, "running container is shut down before delete")
	require.True(t, p.IsRecentlyDestroyed(10050, time.Minute))

	// Idempotent: a missing container is success.
	require.NoError(t, p.Destroy(context.Background(), &VM{VMID: 10050, Node: "pve1", GuestType: config.GuestTypeLXC}))
	state, err = p.PowerState(context.Background(), &VM{VMID: 10050, Node: "pve1", GuestType: config.GuestTypeLXC})
	require.NoError(t, err)
	require.Empty(t, state, "missing container reports unknown")

	require.ErrorIs(t, p.SnapshotCreate(context.Background(), vm, "x"), ErrLXCSnapshotUnsupported)
	require.ErrorIs(t, p.SnapshotRollback(context.Background(), vm, "x"), ErrLXCSnapshotUnsupported)
}

func TestLXC_StartEnsuresFirewall(t *testing.T) {
	t.Parallel()
	f, srv := newLXCFake(t)
	f.cts[10051] = &lxcCT{Name: "c", Config: map[string]any{"net0": "name=eth0,bridge=vmbr0,firewall=0"}}
	p := newLXCProvisioner(t, srv)
	p.cfg.Firewall = config.FirewallConfig{Enabled: true, SecurityGroup: "runners"}
	vm := &VM{VMID: 10051, Node: "pve1", GuestType: config.GuestTypeLXC}
	require.NoError(t, p.Start(context.Background(), vm))
	require.Len(t, f.find(http.MethodPost, "/nodes/pve1/lxc/10051/firewall/rules"), 1)
	put := f.find(http.MethodPut, "/nodes/pve1/lxc/10051/config")
	require.Len(t, put, 1)
	require.Equal(t, "name=eth0,bridge=vmbr0,firewall=1", put[0].Body["net0"])

	// Second start: already sandboxed, no duplicate rule or NIC patch.
	f.cts[10051].Running = false
	require.NoError(t, p.Start(context.Background(), vm))
	require.Len(t, f.find(http.MethodPost, "/nodes/pve1/lxc/10051/firewall/rules"), 1)
	require.Len(t, f.find(http.MethodPut, "/nodes/pve1/lxc/10051/config"), 1)
}

func TestLXC_InjectJITConfigSetsEnvAndReboots(t *testing.T) {
	t.Parallel()
	f, srv := newLXCFake(t)
	f.cts[10060] = &lxcCT{Name: "c", Running: true, Config: map[string]any{}}
	p := newLXCProvisioner(t, srv)
	var logBuf bytes.Buffer
	p.log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	jit := validJIT(t)

	require.NoError(t, p.InjectJITConfig(context.Background(), &VM{VMID: 10060, Node: "pve1", Profile: "ct"}, jit))
	put := f.find(http.MethodPut, "/nodes/pve1/lxc/10060/config")
	require.Len(t, put, 1)
	require.Equal(t, map[string]any{"env": "ACTIONS_RUNNER_JIT_CONFIG=" + jit}, put[0].Body)
	reboot := f.find(http.MethodPost, "/nodes/pve1/lxc/10060/status/reboot")
	require.Len(t, reboot, 1, "a running container is rebooted so init receives the env")
	require.Empty(t, f.find(http.MethodPost, "/nodes/pve1/lxc/10060/status/start"))
	require.False(t, p.jitRestarts.Has(10060), "restart marker cleared after inject")
	require.NotContains(t, logBuf.String(), jit, "the JIT must never be logged")

	got, err := p.ReadJITConfig(context.Background(), &VM{VMID: 10060, Node: "pve1", Profile: "ct"})
	require.NoError(t, err)
	require.Equal(t, jit, string(got))

	for _, r := range f.requests() {
		require.NotContains(t, r.Path, "/agent/", "containers have no guest agent")
	}
}

func TestLXC_InjectJITConfigStartsStoppedContainer(t *testing.T) {
	t.Parallel()
	f, srv := newLXCFake(t)
	f.cts[10061] = &lxcCT{Name: "c", Config: map[string]any{}}
	p := newLXCProvisioner(t, srv)
	require.NoError(t, p.InjectJITConfig(context.Background(), &VM{VMID: 10061, Node: "pve1", GuestType: config.GuestTypeLXC}, validJIT(t)))
	require.Len(t, f.find(http.MethodPost, "/nodes/pve1/lxc/10061/status/start"), 1)
	require.Empty(t, f.find(http.MethodPost, "/nodes/pve1/lxc/10061/status/reboot"))
	require.True(t, f.cts[10061].Running)
}

func TestLXC_InjectJITConfigRejectsBadValues(t *testing.T) {
	t.Parallel()
	f, srv := newLXCFake(t)
	f.cts[10062] = &lxcCT{Name: "c", Running: true, Config: map[string]any{}}
	p := newLXCProvisioner(t, srv)
	vm := &VM{VMID: 10062, Node: "pve1", GuestType: config.GuestTypeLXC}
	for name, bad := range map[string]string{
		"empty":    "",
		"newline":  validJIT(t) + "\n",
		"nul":      "abc\x00def",
		"not-b64":  "not base64!",
		"not-json": base64.StdEncoding.EncodeToString([]byte("plain text")),
	} {
		require.Error(t, p.InjectJITConfig(context.Background(), vm, bad), name)
	}
	require.Empty(t, f.requests(), "rejected values must not reach the API")

	require.Error(t, validateLXCEnvValue("a\x01b"))
	require.Error(t, validateLXCEnvValue("a\x7fb"))
	require.NoError(t, validateLXCEnvValue("QUJD+/=_-"))
}

func TestLXC_InjectJITConfigErrorsRedactSecret(t *testing.T) {
	t.Parallel()
	f, srv := newLXCFake(t)
	f.cts[10063] = &lxcCT{Name: "c", Running: true, Config: map[string]any{}}
	jit := validJIT(t)
	f.failPath["PUT /nodes/pve1/lxc/10063/config"] = "env: invalid value " + jit
	p := newLXCProvisioner(t, srv)
	err := p.InjectJITConfig(context.Background(), &VM{VMID: 10063, Node: "pve1", GuestType: config.GuestTypeLXC}, jit)
	require.Error(t, err)
	require.NotContains(t, err.Error(), jit)
	require.False(t, p.jitRestarts.Has(10063))
	require.Empty(t, f.find(http.MethodPost, "/nodes/pve1/lxc/10063/status/reboot"))
}

func TestLXC_PowerStateMasksJITRestart(t *testing.T) {
	t.Parallel()
	f, srv := newLXCFake(t)
	f.cts[10064] = &lxcCT{Name: "c", Config: map[string]any{}}
	p := newLXCProvisioner(t, srv)
	vm := &VM{VMID: 10064, Node: "pve1", GuestType: config.GuestTypeLXC}
	state, err := p.PowerState(context.Background(), vm)
	require.NoError(t, err)
	require.Equal(t, "stopped", state)
	p.jitRestarts.Set(10064, time.Now(), 0)
	state, err = p.PowerState(context.Background(), vm)
	require.NoError(t, err)
	require.Equal(t, "running", state, "the restart inside InjectJITConfig is not job completion")
}

func TestLXC_ReadJITConfigWithoutEnv(t *testing.T) {
	t.Parallel()
	f, srv := newLXCFake(t)
	f.cts[10065] = &lxcCT{Name: "c", Config: map[string]any{"env": "FOO=bar\x00OTHER=1"}}
	p := newLXCProvisioner(t, srv)
	got, err := p.ReadJITConfig(context.Background(), &VM{VMID: 10065, Node: "pve1", GuestType: config.GuestTypeLXC})
	require.NoError(t, err)
	require.Empty(t, got)

	f.cts[10065].Config["env"] = "FOO=bar\x00ACTIONS_RUNNER_JIT_CONFIG=abc"
	got, err = p.ReadJITConfig(context.Background(), &VM{VMID: 10065, Node: "pve1", GuestType: config.GuestTypeLXC})
	require.NoError(t, err)
	require.Equal(t, "abc", string(got))
}

func TestLXC_WaitReadyTimesOutWhileStopped(t *testing.T) {
	f, srv := newLXCFake(t)
	f.cts[10066] = &lxcCT{Name: "c", Config: map[string]any{}}
	p := newLXCProvisioner(t, srv)
	prev := lxcReadyPollInterval
	lxcReadyPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { lxcReadyPollInterval = prev })
	err := p.WaitReady(context.Background(), &VM{VMID: 10066, Node: "pve1", GuestType: config.GuestTypeLXC}, 150*time.Millisecond)
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out")

	err = p.WaitReady(context.Background(), &VM{VMID: 99999, Node: "pve1", GuestType: config.GuestTypeLXC}, time.Second)
	require.ErrorIs(t, err, ErrVMNotFound)
}

func TestListOwnedVMs_ReturnsQEMUAndLXCClassified(t *testing.T) {
	t.Parallel()
	f, srv := newLXCFake(t)
	owner, err := tags.Initial("test-scaleset", "ct", "")
	require.NoError(t, err)
	vmOwner, err := tags.Initial("test-scaleset", "vm", "")
	require.NoError(t, err)
	f.qemu = fmt.Sprintf(`[{"vmid":10001,"name":"q","status":"running","tags":%q},{"vmid":500,"name":"human","status":"running","tags":""}]`, tags.Encode(vmOwner))
	f.cts[10002] = &lxcCT{Name: "c", Tags: tags.Encode(owner), Config: map[string]any{}}
	f.cts[10003] = &lxcCT{Name: "gh-runner-test-scaleset-10003", Config: map[string]any{}} // untagged orphan in range
	f.cts[600] = &lxcCT{Name: "someone-elses", Config: map[string]any{}}
	p := newLXCProvisioner(t, srv)
	p.vmNamePrefix = "gh-runner-test-scaleset-"
	p.cfg.VMIDRange = config.VMIDRange{Min: 10000, Max: 19999}

	vms, err := p.ListOwnedVMs(context.Background())
	require.NoError(t, err)
	byID := map[int]*VM{}
	for _, v := range vms {
		byID[v.VMID] = v
	}
	require.Len(t, byID, 3)
	require.Equal(t, config.GuestTypeQEMU, byID[10001].GuestType)
	require.Equal(t, "vm", byID[10001].Profile)
	require.Equal(t, config.GuestTypeLXC, byID[10002].GuestType)
	require.Equal(t, "ct", byID[10002].Profile)
	require.Equal(t, config.GuestTypeLXC, byID[10003].GuestType)
	require.Empty(t, byID[10003].Profile)
}

func TestResolveGuestType(t *testing.T) {
	t.Parallel()
	f, srv := newLXCFake(t)
	f.cts[10070] = &lxcCT{Name: "c", Config: map[string]any{}}

	// A QEMU-only scale set never probes.
	qp := newTestProvisioner(t, srv, "pve1")
	qp.profiles = map[string]config.ProfileConfig{"default": {Name: "default"}}
	require.Equal(t, config.GuestTypeQEMU, qp.resolveGuestType(context.Background(), &VM{VMID: 10070, Node: "pve1"}))
	require.Empty(t, f.requests())

	p := newLXCProvisioner(t, srv)
	require.Equal(t, config.GuestTypeLXC, p.resolveGuestType(context.Background(), &VM{VMID: 1, Node: "pve1", GuestType: config.GuestTypeLXC}))
	require.Equal(t, config.GuestTypeQEMU, p.resolveGuestType(context.Background(), &VM{VMID: 1, Node: "pve1", Profile: "vm"}))
	require.Equal(t, config.GuestTypeLXC, p.resolveGuestType(context.Background(), &VM{VMID: 1, Node: "pve1", Profile: "ct"}))
	require.Empty(t, f.requests(), "explicit type and known profile resolve without API calls")
	// No profile: probe.
	require.Equal(t, config.GuestTypeLXC, p.resolveGuestType(context.Background(), &VM{VMID: 10070, Node: "pve1"}))
	require.Equal(t, config.GuestTypeQEMU, p.resolveGuestType(context.Background(), &VM{VMID: 10071, Node: "pve1"}))

	require.Equal(t, config.GuestTypeLXC, p.templateGuestType(9100))
	require.Equal(t, config.GuestTypeQEMU, p.templateGuestType(9000))
}

func TestLXCTemplateDiscovery(t *testing.T) {
	t.Parallel()
	f, srv := newLXCFake(t)
	f.cts[9100] = &lxcCT{Name: "tmpl", Template: true, Config: map[string]any{}}
	f.cts[9101] = &lxcCT{Name: "not-a-template", Config: map[string]any{}}

	// An all-LXC scale set discovers its container template at startup
	// instead of the global (QEMU) proxmox.template_vmid.
	p := newLXCProvisioner(t, srv)
	p.profiles = map[string]config.ProfileConfig{"ct": {Name: "ct", GuestType: config.GuestTypeLXC, TemplateVMID: 9100}}
	p.templateNode = ""
	require.NoError(t, p.discoverTemplateNode(context.Background()))
	require.Equal(t, "pve1", p.templateNode)
	for _, r := range f.requests() {
		require.NotContains(t, r.Path, "/qemu/")
	}

	p.profiles = map[string]config.ProfileConfig{"ct": {Name: "ct", GuestType: config.GuestTypeLXC, TemplateVMID: 9101}}
	err := p.discoverTemplateNode(context.Background())
	require.ErrorIs(t, err, ErrTemplateNotFound)
	require.Contains(t, err.Error(), "none are templates")

	p = newLXCProvisioner(t, srv)
	node, err := p.locateTemplate(context.Background(), 9100)
	require.NoError(t, err)
	require.Equal(t, "pve1", node)
}
