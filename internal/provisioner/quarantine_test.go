package provisioner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQuarantineUsesProtectionAndDigestTogether(t *testing.T) {
	var options map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/nodes/pve1/status":
			_, _ = w.Write([]byte(`{"data":{"node":"pve1","status":"online"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/nodes/pve1/qemu/10001/status/current":
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","vmid":10001}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/nodes/pve1/qemu/10001/config":
			_, _ = w.Write([]byte(`{"data":{"digest":"abc123","tags":"gh-scaleset-test;keep"}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/nodes/pve1/qemu/10001/config":
			if err := json.NewDecoder(r.Body).Decode(&options); err != nil {
				t.Errorf("decode config request: %v", err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"data":null}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p := newTestProvisioner(t, srv, "pve1")
	require.NoError(t, p.Quarantine(context.Background(), &VM{VMID: 10001, Node: "pve1"}, time.Unix(1800000000, 0)))
	require.Equal(t, "abc123", options["digest"])
	require.Equal(t, float64(1), options["protection"])
	require.Contains(t, options["tags"], "keep")
	require.Contains(t, options["tags"], "gh-quarantine-until-1800000000")
}

func TestQuarantineRetryPreservesExistingDeadline(t *testing.T) {
	var options map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/nodes/pve1/status":
			_, _ = w.Write([]byte(`{"data":{"node":"pve1","status":"online"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/nodes/pve1/qemu/10001/status/current":
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","vmid":10001}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/nodes/pve1/qemu/10001/config":
			_, _ = w.Write([]byte(`{"data":{"digest":"abc123","tags":"gh-quarantine-until-1800000000"}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/nodes/pve1/qemu/10001/config":
			if err := json.NewDecoder(r.Body).Decode(&options); err != nil {
				t.Errorf("decode config request: %v", err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"data":null}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p := newTestProvisioner(t, srv, "pve1")
	require.NoError(t, p.Quarantine(context.Background(), &VM{VMID: 10001, Node: "pve1"}, time.Unix(1800003600, 0)))
	require.Equal(t, "gh-quarantine-until-1800000000", options["tags"])
	require.Equal(t, float64(1), options["protection"])
}

func TestQuarantineProtectsRunningVMWithoutStoppingIt(t *testing.T) {
	var putCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/nodes/pve1/status":
			_, _ = w.Write([]byte(`{"data":{"node":"pve1","status":"online"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/nodes/pve1/qemu/10001/status/current":
			_, _ = w.Write([]byte(`{"data":{"status":"running","vmid":10001}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/nodes/pve1/qemu/10001/config":
			_, _ = w.Write([]byte(`{"data":{"digest":"abc123","tags":"keep"}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/nodes/pve1/qemu/10001/config":
			putCount++
			_, _ = w.Write([]byte(`{"data":null}`))
		default:
			t.Errorf("unexpected action %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p := newTestProvisioner(t, srv, "pve1")
	require.NoError(t, p.Quarantine(context.Background(), &VM{VMID: 10001, Node: "pve1"}, time.Unix(1800000000, 0)))
	require.Equal(t, 1, putCount)
}

func TestQuarantineVMIDFreeUsesClusterAllocator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/cluster/status":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case "/cluster/nextid":
			if r.URL.Query().Get("vmid") != "10001" {
				t.Errorf("missing VMID on cluster check: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"data":"10001"}`))
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p := newTestProvisioner(t, srv, "pve1")
	free, err := p.QuarantineVMIDFree(context.Background(), 10001)
	require.NoError(t, err)
	require.True(t, free)
}

func TestQuarantineVMIDFreeFailsClosedOnClusterError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/cluster/status" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errors":"unavailable"}`))
	}))
	defer srv.Close()
	p := newTestProvisioner(t, srv, "pve1")
	free, err := p.QuarantineVMIDFree(context.Background(), 10001)
	require.Error(t, err)
	require.False(t, free)
}
