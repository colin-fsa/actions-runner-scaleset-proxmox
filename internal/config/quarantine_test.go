package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQuarantineRejectsMultipleWindowsScaleSets(t *testing.T) {
	c := Config{
		Pool: PoolConfig{QuarantineSuspiciousWindows: true, QuarantineJournalDir: "/state"},
		Scalesets: []ScaleSetEntry{
			{Profiles: []ProfileConfig{{GuestOS: "windows"}}},
			{Profiles: []ProfileConfig{{GuestOS: "windows"}}},
		},
	}
	require.ErrorContains(t, c.resolvePool(), "at most one Windows scale set")
}
