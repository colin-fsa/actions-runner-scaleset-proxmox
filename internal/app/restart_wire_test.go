package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/pool"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/tags"
	proxmox "github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
)

type restartWireTransport func(*http.Request) (*http.Response, error)

func (f restartWireTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type lxcWireReader struct {
	*recoveryFixture
	sdk *proxmox.Client
}

func (r lxcWireReader) Get(ctx context.Context, path string, dst any) error {
	if strings.HasPrefix(path, "/nodes/pve/lxc") {
		return r.sdk.Get(ctx, path, dst)
	}
	return r.recoveryFixture.Get(ctx, path, dst)
}

func TestScopedRestartReadsLXCQuotedCPUAndVMID(t *testing.T) {
	cutoff := time.Now().Add(-time.Minute).Unix()
	f := &recoveryFixture{active: []restartTask{}, archive: []restartTask{},
		tail: []restartTask{receipt(proofToken, "vzstart", "11001", cutoff-10, cutoff-5)}}
	var cfg config.Config
	cfg.Nodes.SingleNode = "pve"
	cfg.Proxmox.Auth.TokenID = proofToken
	cfg.Scalesets = []config.ScaleSetEntry{{Name: "lin", VMIDRange: &config.VMIDRange{Min: 11001, Max: 11009},
		Profiles: []config.ProfileConfig{{Name: "default", GuestType: "lxc"}}}}
	owner, err := tags.OwnerTag("lin")
	require.NoError(t, err)
	const name = "gh-runner-lin-11001"
	wireTags := tags.Encode([]string{tags.Marker, owner, tags.ProfileTag("default")})
	managed := fmt.Sprintf(`"name":%q,"tags":%q,"status":"running","pid":1234,"maxmem":10737418240`, name, wireTags)
	list := func(id, cpu string) string {
		// Include a non-runner capped CT and the stopped golden template;
		// the node inventory covers the whole node, not just our runners.
		return fmt.Sprintf(`[{"vmid":%s,"cpus":%s,"uptime":%d,%s},{"vmid":107,"name":"dev","cpus":"6","status":"running"},{"vmid":"11000","name":"template","cpus":"6","status":"stopped","template":1}]`, id, cpu, f.advance+25, managed)
	}
	current := func(id, cpu, extra string) string {
		return fmt.Sprintf(`{"vmid":%s,"cpus":%s,"uptime":%d,%s%s}`, id, cpu, f.advance+25, managed, extra)
	}
	listBody, currentBody, listCode := "", "", http.StatusOK
	reader := lxcWireReader{f, proxmox.NewClient("http://pve.invalid", proxmox.WithHTTPClient(&http.Client{
		Transport: restartWireTransport(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				return nil, errors.New("unexpected non-read")
			}
			body, code := "", http.StatusOK
			switch req.URL.Path {
			case "/nodes/pve/lxc":
				body, code = listBody, listCode
				if code == http.StatusOK {
					body = `{"data":` + list(`"11001"`, `"5.5"`) + `}`
					if listBody != "" {
						body = `{"data":` + listBody + `}`
					}
				}
			case "/nodes/pve/lxc/11001/status/current":
				if currentBody == "" {
					body = `{"data":` + current("11001", `"5.5"`, `,"ha":{"managed":0}`) + `}`
				} else {
					body = `{"data":` + currentBody + `}`
				}
			default:
				return nil, errors.New("unexpected read")
			}
			return &http.Response{StatusCode: code, Status: http.StatusText(code), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		}),
	}))}
	generation := time.Now()
	adopted := map[string][]pool.RowSnapshot{"lin": {{VMID: 11001, Node: "pve", Name: name, Profile: "default",
		State: "assigned", CreatedAt: generation}}}
	var out restartResult
	err = proveScopedRestart(t.Context(), reader, &cfg, adopted, func(context.Context, *provisioner.VM) error {
		return errors.New("guest has not demonstrated readiness")
	}, cutoff, 0, &out)
	require.NoError(t, err)
	require.Len(t, out.holds, 1)
	require.Equal(t, generation, out.holds[0].Generation)
	require.Equal(t, 6, out.holds[0].Shape.VCPUs)
	require.EqualValues(t, 10737418240, out.holds[0].Shape.MemoryBytes)

	bad := []struct{ body, why string }{
		{list(`"11001.1"`, "6"), "invalid vmid encoding"},
		{list("1.1001e4", "6"), "invalid vmid encoding"},
		{list(`"011001"`, "6"), "invalid vmid encoding"},
		{list("null", "6"), "invalid vmid encoding"},
		{list("11001", `"opaque"`), "invalid cpus encoding"},
		{list("11001", `"6 "`), "invalid cpus encoding"},
		{list("11001", "null"), "invalid cpus encoding"},
		{"null", "missing JSON list"},
		{`{"foo":"bar"}`, "unexpected JSON shape"},
	}
	for _, tc := range bad {
		listBody = tc.body
		_, err := readRestartView(t.Context(), reader, &cfg, cutoff)
		require.ErrorContains(t, err, "unavailable lxc inventory")
		require.ErrorContains(t, err, tc.why)
	}
	listBody = list("11001", `"6"`) // isolate the actual cpulimit-only wire defect
	_, err = readRestartView(t.Context(), reader, &cfg, cutoff)
	require.NoError(t, err)
	listBody = ""
	for _, body := range []string{current("11002", "6", `,"ha":{"managed":0}`), current("11001", "6", ""), current("11001", "6", `,"ha":{"managed":1}`)} {
		currentBody = body
		_, err := readRestartView(t.Context(), reader, &cfg, cutoff)
		require.ErrorContains(t, err, "guest 11001 HA, process or power is ambiguous")
	}
	currentBody = current("11001", `false`, `,"ha":{"managed":0}`)
	_, err = readRestartView(t.Context(), reader, &cfg, cutoff)
	require.ErrorContains(t, err, "guest 11001 direct status unavailable: invalid cpus encoding")

	listCode, listBody = http.StatusBadRequest, `{"errors":{"secret":"must-not-appear"}}`
	_, err = readRestartView(t.Context(), reader, &cfg, cutoff)
	require.EqualError(t, err, "unavailable lxc inventory: API read failed")
	require.ErrorContains(t, errors.Unwrap(err), "bad request")
}
