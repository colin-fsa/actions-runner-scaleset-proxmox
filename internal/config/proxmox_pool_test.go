package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
)

func TestProfiles_ProxmoxPoolParses(t *testing.T) {
	setEnv(t, map[string]string{"TEST_GH_TOKEN": "ghp_fake", "TEST_PVE_TOKEN": "pve-secret"})
	input := strings.Replace(validProfileYAML, "template_vmid: 9001", "template_vmid: 9001\n    proxmox_pool: ci-runners", 1)
	cfg, err := config.Parse([]byte(input))
	require.NoError(t, err)
	require.Equal(t, "ci-runners", cfg.Profiles[0].ProxmoxPool)
	require.Empty(t, cfg.Profiles[1].ProxmoxPool)
	require.Equal(t, "ci-runners", cfg.Scalesets[0].Profiles[0].ProxmoxPool)
}
