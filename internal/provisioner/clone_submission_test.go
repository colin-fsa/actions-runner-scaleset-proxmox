package provisioner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/testutil/fakeproxmox"
	"github.com/stretchr/testify/require"
)

func TestCloneSubmissionGatesActualPostForQEMUAndLXC(t *testing.T) {
	for _, kind := range []string{"qemu", "lxc"} {
		t.Run(kind, func(t *testing.T) {
			var server *httptest.Server
			var p *pmox
			if kind == "qemu" {
				server = fakeproxmox.New(t, fakeproxmox.Options{}).Server
			} else {
				f, s := newLXCFake(t)
				server = s
				f.cts[9100] = &lxcCT{Name: "tmpl", Template: true, Config: map[string]any{}}
			}
			var gated atomic.Bool
			var posts atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/clone") {
					if !gated.Load() {
						t.Error("clone POST bypassed submission gate")
					}
					posts.Add(1)
				}
				server.Config.Handler.ServeHTTP(w, r)
			}))
			defer proxy.Close()
			// Keep the real provisioner's profile/template settings, changing only
			// the HTTP endpoint so the assertion sees the actual library POST.
			if kind == "qemu" {
				p = newTestProvisioner(t, proxy, "pve1")
			} else {
				p = newLXCProvisioner(t, proxy)
			}
			opts := CloneOptions{NewVMID: 10042, Node: "pve1", Name: "runner", Linked: true, GuestType: kind}
			if kind == "lxc" {
				opts.Profile = "ct"
				opts.TemplateVMID = 9100
			}
			refused := errors.New("leader fence closed")
			opts.Submission = func(context.Context, func(context.Context) error) error { return refused }
			_, err := p.Clone(t.Context(), opts)
			require.ErrorIs(t, err, refused)
			require.Zero(t, posts.Load(), "a rejected gate must issue no clone POST")
			opts.Submission = func(ctx context.Context, submit func(context.Context) error) error {
				gated.Store(true)
				defer gated.Store(false)
				return submit(ctx)
			}
			_, err = p.Clone(t.Context(), opts)
			require.NoError(t, err)
			require.EqualValues(t, 1, posts.Load())
		})
	}
}
