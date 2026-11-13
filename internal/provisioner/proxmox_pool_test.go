package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/testutil/fakeproxmox"
)

// Exercise the real Clone method and go-proxmox serialization, including the
// linked-clone early return. No pool means the wire field must be absent.
func TestClone_ProxmoxPoolRequest(t *testing.T) {
	for _, linked := range []bool{false, true} {
		for _, pool := range []string{"", "ci-runners"} {
			name := "full/" + pool
			if linked {
				name = "linked/" + pool
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				fp := fakeproxmox.New(t, fakeproxmox.Options{})
				bodies := make(chan map[string]any, 1)
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/clone") {
						raw, err := io.ReadAll(r.Body)
						if err != nil {
							http.Error(w, err.Error(), http.StatusBadRequest)
							return
						}
						var body map[string]any
						if err := json.Unmarshal(raw, &body); err != nil {
							http.Error(w, err.Error(), http.StatusBadRequest)
							return
						}
						bodies <- body
						r.Body = io.NopCloser(bytes.NewReader(raw))
					}
					fp.Config.Handler.ServeHTTP(w, r)
				}))
				defer proxy.Close()
				p := newTestProvisioner(t, proxy, "pve1")
				vm, err := p.Clone(context.Background(), CloneOptions{NewVMID: 10042, Node: "pve1", Name: "test", Linked: linked, ProxmoxPool: pool})
				require.NoError(t, err)
				require.Equal(t, 10042, vm.VMID)
				body := <-bodies
				if pool == "" {
					require.NotContains(t, body, "pool")
				} else {
					require.Equal(t, pool, body["pool"])
				}
			})
		}
	}
}

func TestClone_ProxmoxPoolRejectionFailsClosed(t *testing.T) {
	t.Parallel()
	fp := fakeproxmox.New(t, fakeproxmox.Options{})
	calls := make(chan map[string]any, 4)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/clone") {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			calls <- body
			http.Error(w, `{"errors":"pool permission denied"}`, http.StatusForbidden)
			return
		}
		fp.Config.Handler.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	p := newTestProvisioner(t, proxy, "pve1")
	vm, err := p.Clone(context.Background(), CloneOptions{NewVMID: 10042, Node: "pve1", Name: "test", Linked: true, ProxmoxPool: "restricted"})
	require.ErrorContains(t, err, "issue clone")
	require.Nil(t, vm)
	require.Len(t, calls, 1, "must not retry without the selected pool")
	require.Equal(t, "restricted", (<-calls)["pool"])
	require.Zero(t, p.InFlightCloneCount())
	for _, got := range fp.Snapshot() {
		require.NotEqual(t, 10042, got.VMID)
	}
}
