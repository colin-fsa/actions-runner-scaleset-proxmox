package provisioner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
)

func TestPacedTaskReceiptRequiresExactTerminalSuccess(t *testing.T) {
	cases := []struct {
		actual proxmox.UPID
		status string
		exit   string
		want   bool
	}{
		{"expected", "stopped", "OK", true},
		{"expected", "stopped", "", false},
		{"expected", "", "OK", false},
		{"expected", "unexpected", "OK", false},
		{"expected", "running", "OK", false},
		{"expected", "stopped", "aborted", false},
		{"other", "stopped", "OK", false},
	}
	for _, c := range cases {
		require.Equal(t, c.want, startTaskSucceeded(&proxmox.Task{UPID: c.actual, Status: c.status, ExitStatus: c.exit}, "expected"))
	}
}

func TestPacedPVEEmptyTaskReceiptIsAmbiguous(t *testing.T) {
	for _, kind := range []string{"qemu", "lxc"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/status/start"):
					_, _ = io.WriteString(w, `{"data":""}`)
				case strings.HasSuffix(r.URL.Path, "/status/current"):
					_, _ = io.WriteString(w, `{"data":{"vmid":10042,"name":"x","status":"stopped"}}`)
				case r.URL.Path == "/nodes/pve1/status" || strings.HasSuffix(r.URL.Path, "/config"):
					_, _ = io.WriteString(w, `{"data":{}}`)
				default:
					t.Errorf("unexpected request after ambiguous result: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer srv.Close()
			p := newTestProvisioner(t, srv, "pve1")
			gated := false
			err := p.StartWithSubmission(t.Context(), &VM{VMID: 10042, Node: "pve1", Name: "x", GuestType: kind}, func(ctx context.Context, submit func(context.Context) error) error {
				gated = true
				return submit(ctx)
			})
			require.True(t, gated)
			require.ErrorContains(t, err, "no task ID; outcome unknown")
		})
	}
}
