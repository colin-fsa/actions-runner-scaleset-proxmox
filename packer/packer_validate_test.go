//go:build packer

// Package packer hosts a tag-gated test that shells out to the Packer
// CLI to syntax-check the HCL in this directory. The build tag keeps it
// out of the default `go test ./...` run; CI invokes it via
// `make e2e-packer`.
package packer

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPackerValidateSyntax runs `packer validate -syntax-only` against
// the packer/ directory. We use -syntax-only because the variables file
// is gitignored — the syntactic check is what catches HCL drift in CI
// without needing Proxmox credentials or a vars file.
//
// Skips when the `packer` CLI is not on PATH, so contributors without
// Packer installed can still run the regular `go test ./...`.
func TestPackerValidateSyntax(t *testing.T) {
	if _, err := exec.LookPath("packer"); err != nil {
		t.Skip("packer CLI not installed; install HashiCorp Packer to run this test")
	}

	// Locate the packer/ directory relative to this test file so the
	// command works regardless of where `go test` is invoked from.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	packerDir := filepath.Dir(thisFile)

	// The Windows build lives in its own directory (own variable namespace).
	for _, dir := range []string{packerDir, filepath.Join(packerDir, "windows")} {
		cmd := exec.Command("packer", "validate", "-syntax-only", dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("packer validate %s failed: %v\n%s", dir, err, out)
		}
		if !strings.Contains(string(out), "passed") {
			t.Fatalf("packer validate %s output unexpected; want 'passed' marker:\n%s", dir, out)
		}
	}
}
