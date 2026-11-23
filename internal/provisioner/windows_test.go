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

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
)

func TestWindowsJITHandoffAndReadback(t *testing.T) {
	var calls []map[string]any
	var readPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/file-write"), strings.HasSuffix(r.URL.Path, "/exec"):
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			calls = append(calls, body)
			if strings.HasSuffix(r.URL.Path, "/exec") {
				_, _ = io.WriteString(w, `{"data":{"pid":7}}`)
			} else {
				_, _ = io.WriteString(w, `{"data":null}`)
			}
		case strings.HasSuffix(r.URL.Path, "/exec-status"):
			_, _ = io.WriteString(w, `{"data":{"exited":true,"exitcode":0}}`)
		case strings.HasSuffix(r.URL.Path, "/file-read"):
			readPath = r.URL.Query().Get("file")
			_, _ = io.WriteString(w, `{"data":{"content":"raw"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := newTestProvisioner(t, srv, "pve1")
	p.profiles = map[string]config.ProfileConfig{"win": {Name: "win", GuestOS: "windows", WindowsJITPath: `C:\handoff\jit`}}
	vm := &VM{VMID: 7, Node: "pve1", Profile: "win"}
	jit := "eyJydW5uZXJfaWQiOjQyfQ=="
	require.NoError(t, p.InjectJITConfig(t.Context(), vm, jit))
	require.Equal(t, `C:\handoff\jit.tmp`, calls[0]["file"])
	require.Equal(t, jit, calls[0]["content"])
	cmd := calls[1]["command"].([]any)
	require.Equal(t, "powershell.exe", cmd[0])
	require.Equal(t, "-EncodedCommand", cmd[3])
	require.NotContains(t, cmd[4], jit)
	encoded, err := base64.StdEncoding.DecodeString(cmd[4].(string))
	require.NoError(t, err)
	units := make([]uint16, len(encoded)/2)
	for i := range units {
		units[i] = uint16(encoded[2*i]) | uint16(encoded[2*i+1])<<8
	}
	script := string(utf16.Decode(units))
	require.Contains(t, script, `C:\handoff\jit.tmp`)
	require.Contains(t, script, "ReadAllText")
	require.Contains(t, script, "-cne")
	require.Contains(t, script, "[System.IO.File]::Move")
	data, err := p.ReadJITConfig(t.Context(), vm)
	require.NoError(t, err)
	require.Equal(t, "raw", string(data))
	require.Equal(t, `C:\handoff\jit`, readPath)
	_, _, err = p.guestJITSettings(&VM{Profile: "missing"})
	require.Error(t, err)
}

func TestWindowsJITWriteErrorDoesNotExposePayload(t *testing.T) {
	const jit = "eyJydW5uZXJfaWQiOjQyfQ=="
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, jit, http.StatusBadRequest)
	}))
	defer srv.Close()
	p := newTestProvisioner(t, srv, "pve1")
	p.profiles = map[string]config.ProfileConfig{"win": {Name: "win", GuestOS: "windows"}}
	err := p.InjectJITConfig(t.Context(), &VM{VMID: 7, Node: "pve1", Profile: "win"}, jit)
	require.Error(t, err)
	require.NotContains(t, err.Error(), jit)
}

func TestWindowsJITWriteDiagnosticsAreRedacted(t *testing.T) {
	for _, tc := range []struct{ cause, hint string }{
		{"No such file or directory", "prepare the template"},
		{"Access is denied", "ACL"},
		{"command is disabled", "disabled or unsupported"},
		{"unexpected error", "response suppressed"},
	} {
		err := windowsJITWriteError(fmt.Errorf("%s: secret-jit-payload", tc.cause))
		require.Contains(t, err.Error(), tc.hint)
		require.NotContains(t, err.Error(), "secret-jit-payload")
	}
}

func TestWindowsJITHandoffState(t *testing.T) {
	for _, tc := range []struct {
		name string
		exit int
		want string
	}{
		{"pristine", 0, "pristine"},
		{"occupied", 41, "occupied"},
		{"unknown", 42, "unknown"},
		{"no exit code", -1, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/agent/exec"):
					var body struct{ Command []string }
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					require.Len(t, body.Command, 5)
					buf, err := base64.StdEncoding.DecodeString(body.Command[4])
					require.NoError(t, err)
					units := make([]uint16, len(buf)/2)
					for i := range units {
						units[i] = uint16(buf[2*i]) | uint16(buf[2*i+1])<<8
					}
					script := string(utf16.Decode(units))
					for _, s := range []string{"Get-ScheduledTask", "Get-ChildItem", "started", "jitconfig.tmp", "ReparsePoint", "Test-JitConfig.ps1", ".Execute -ine $expectedExe"} {
						require.Contains(t, script, s)
					}
					require.NotContains(t, script, "ReadAllText")
					require.NotContains(t, script, "Get-Content")
					_, _ = io.WriteString(w, `{"data":{"pid":7}}`)
				case strings.HasSuffix(r.URL.Path, "/agent/exec-status"):
					if tc.exit == -1 {
						_, _ = io.WriteString(w, `{"data":{"exited":true,"err-data":"must-not-leak"}}`)
					} else {
						_, _ = fmt.Fprintf(w, `{"data":{"exited":true,"exitcode":%d,"err-data":"must-not-leak"}}`, tc.exit)
					}
				default:
					t.Errorf("unexpected read path: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			p := newTestProvisioner(t, srv, "pve1")
			p.profiles = map[string]config.ProfileConfig{"win": {Name: "win", GuestOS: "windows"}}
			got, err := p.WindowsJITHandoffState(t.Context(), &VM{VMID: 7, Node: "pve1", Profile: "win"})
			require.Equal(t, tc.want, got)
			if tc.want == "unknown" {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "must-not-leak")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
