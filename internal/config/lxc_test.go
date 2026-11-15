package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProfileGuestTypeValidation(t *testing.T) {
	lxc := func(mod func(*ProfileConfig)) ProfileConfig {
		p := ProfileConfig{Name: "ct", GuestType: GuestTypeLXC, TemplateVMID: 9200}
		if mod != nil {
			mod(&p)
		}
		return p
	}
	cases := []struct {
		name     string
		recycle  string
		profiles []ProfileConfig
		wantErr  string
	}{
		{"qemu default", RecycleModeDestroy, []ProfileConfig{{Name: "default"}}, ""},
		{"qemu explicit", RecycleModeDestroy, []ProfileConfig{{Name: "default", GuestType: GuestTypeQEMU}}, ""},
		{"lxc", RecycleModeDestroy, []ProfileConfig{lxc(nil)}, ""},
		{"lxc with empty recycle mode", "", []ProfileConfig{lxc(nil)}, ""},
		{"lxc and qemu mixed under destroy", RecycleModeDestroy, []ProfileConfig{lxc(nil), {Name: "vm"}}, ""},
		{"all-lxc scale set under snapshot_rollback", RecycleModeSnapshotRollback, []ProfileConfig{lxc(nil)}, ""},
		{"lxc mixed with qemu under snapshot_rollback", RecycleModeSnapshotRollback, []ProfileConfig{lxc(nil), {Name: "vm"}}, "cannot share a scale set"},
		{"unknown type", RecycleModeDestroy, []ProfileConfig{{Name: "x", GuestType: "kvm"}}, "is not supported"},
		{"lxc without template", RecycleModeDestroy, []ProfileConfig{lxc(func(p *ProfileConfig) { p.TemplateVMID = 0 })}, "explicit template_vmid"},
		{"lxc with network", RecycleModeDestroy, []ProfileConfig{lxc(func(p *ProfileConfig) { p.Network = &ProfileNetworkConfig{Bridge: "vmbr1"} })}, "network block"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				Pool:      PoolConfig{RecycleMode: tc.recycle},
				Scalesets: []ScaleSetEntry{{Name: "test", MaxConcurrentRunners: 1, Profiles: tc.profiles}},
			}
			err := cfg.validateProfiles()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestRecycleModeFor(t *testing.T) {
	cfg := Config{Pool: PoolConfig{RecycleMode: RecycleModeSnapshotRollback}}
	ct := ProfileConfig{Name: "ct", GuestType: GuestTypeLXC, TemplateVMID: 9200}
	require.Equal(t, RecycleModeDestroy, cfg.RecycleModeFor(ScaleSetEntry{Profiles: []ProfileConfig{ct}}))
	require.Equal(t, RecycleModeSnapshotRollback, cfg.RecycleModeFor(ScaleSetEntry{Profiles: []ProfileConfig{{Name: "vm"}}}))
	require.Equal(t, RecycleModeSnapshotRollback, cfg.RecycleModeFor(ScaleSetEntry{Profiles: []ProfileConfig{ct, {Name: "vm"}}}))
	require.Equal(t, GuestTypeQEMU, ProfileConfig{}.GuestTypeOrDefault())
}

// The shipped LXC example loads through the public loader.
func TestLXCLinuxExample(t *testing.T) {
	t.Setenv("SCALESET_GITHUB_PAT_TOKEN", "example-test-token")
	t.Setenv("SCALESET_PROXMOX_AUTH_TOKEN_SECRET", "example-test-secret")
	data, err := os.ReadFile("../../examples/lxc-linux.yaml")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Scalesets, 2)
	require.Equal(t, GuestTypeLXC, cfg.Scalesets[0].Profiles[0].GuestType)
	require.Equal(t, RecycleModeDestroy, cfg.RecycleModeFor(cfg.Scalesets[0]))
	require.Equal(t, RecycleModeSnapshotRollback, cfg.RecycleModeFor(cfg.Scalesets[1]))
}
