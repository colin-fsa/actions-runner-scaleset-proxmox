package provisioner

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/require"
)

func decodePS(t *testing.T, enc string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(enc)
	require.NoError(t, err)
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

func TestPrepareWindowsSequenceAndPasswordHandling(t *testing.T) {
	const password = "S3cret'pw$1"
	type call struct {
		kind string
		body map[string]any
	}
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/file-write"), strings.HasSuffix(r.URL.Path, "/exec"):
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			kind := "write"
			if strings.HasSuffix(r.URL.Path, "/exec") {
				kind = "exec"
				_, _ = io.WriteString(w, `{"data":{"pid":7}}`)
			} else {
				_, _ = io.WriteString(w, `{"data":null}`)
			}
			calls = append(calls, call{kind, body})
		case strings.HasSuffix(r.URL.Path, "/exec-status"):
			_, _ = io.WriteString(w, `{"data":{"exited":true,"exitcode":0}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := newTestProvisioner(t, srv, "pve1")
	vm := &VM{VMID: 120, Node: "pve1"}
	require.NoError(t, p.prepareWindows(t.Context(), vm, PrepareWindowsOptions{
		VMID: 120, UserName: "RunnerUI", Password: password, RunnerDirectory: `C:\actions-runner`,
	}))

	var writes []string
	var execs []string
	for _, c := range calls {
		if c.kind == "write" {
			writes = append(writes, c.body["file"].(string))
			if strings.HasSuffix(c.body["file"].(string), `\password`) {
				require.Equal(t, password, c.body["content"])
			} else {
				require.NotContains(t, c.body["content"], password)
			}
			continue
		}
		cmd := c.body["command"].([]any)
		var flat []string
		for _, a := range cmd {
			flat = append(flat, a.(string))
		}
		joined := strings.Join(flat, " ")
		require.NotContains(t, joined, password, "password must never appear in a command line")
		if flat[3] == "-EncodedCommand" {
			joined = decodePS(t, flat[4])
			require.NotContains(t, joined, password)
		}
		execs = append(execs, joined)
	}
	require.Equal(t, []string{
		prepareGuestDir + `\Install-JitRunnerWatcher.ps1`,
		prepareGuestDir + `\Watch-JitRunner.ps1`,
		prepareGuestDir + `\Test-JitConfig.ps1`,
		prepareGuestDir + `\Initialize-InteractiveRunner.ps1`,
		prepareGuestDir + `\password`,
	}, writes)
	require.Len(t, execs, 5) // mkdir, init, install, verify, cleanup
	require.Contains(t, execs[0], "SetAccessRuleProtection")
	require.Contains(t, execs[1], "Initialize-InteractiveRunner.ps1 -UserName RunnerUI -PasswordFile "+prepareGuestDir+`\password`)
	require.Contains(t, execs[2], `Install-JitRunnerWatcher.ps1 -InteractiveUser .\RunnerUI -RunnerDirectory C:\actions-runner`)
	require.Contains(t, execs[3], "Get-ScheduledTask")
	require.Contains(t, execs[4], "Remove-Item")
}

func TestPrepareWindowsRedactsPasswordFromErrors(t *testing.T) {
	const password = "hunter2hunter2"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			_, _ = io.WriteString(w, `{"data":{"pid":1}}`)
		case strings.HasSuffix(r.URL.Path, "/exec-status"):
			_, _ = fmt.Fprintf(w, `{"data":{"exited":true,"exitcode":1,"err-data":"bad %s"}}`, password)
		default:
			_, _ = io.WriteString(w, `{"data":null}`)
		}
	}))
	defer srv.Close()
	p := newTestProvisioner(t, srv, "pve1")
	err := p.prepareWindows(t.Context(), &VM{VMID: 1, Node: "pve1"}, PrepareWindowsOptions{UserName: "RunnerUI", Password: password})
	require.Error(t, err)
	require.NotContains(t, err.Error(), password)
}

func TestPrepareWindowsTemplateValidatesInput(t *testing.T) {
	for _, o := range []PrepareWindowsOptions{
		{UserName: "x", Password: "p"},                                  // no vmid
		{VMID: 1, UserName: "bad name", Password: "p"},                  // bad user
		{VMID: 1, UserName: "averyveryverylongusername", Password: "p"}, // too long
		{VMID: 1, UserName: "ok"},                                       // no password
	} {
		require.Error(t, PrepareWindowsTemplate(t.Context(), newTestProvisioner(t, httptest.NewServer(http.NotFoundHandler()), "n").cfg, o, nil))
	}
}
