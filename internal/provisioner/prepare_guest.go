package provisioner

import (
	"context"
	"encoding/base64"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
	windowsscripts "github.com/jeffresc/actions-runner-scaleset-proxmox/scripts/windows"
)

// PrepareWindowsOptions configures [PrepareWindowsTemplate].
type PrepareWindowsOptions struct {
	// VMID is the running, not-yet-templated Windows VM to prepare. It must
	// already have the runner installed and the QEMU guest agent running.
	VMID int
	// UserName is the local Administrator account that will own the
	// interactive session; it is created or updated.
	UserName string
	// Password for UserName. Used for the account and automatic logon; it is
	// written to the guest only through a temporary protected file.
	Password string
	// RunnerDirectory and JitPath are passed to Install-JitRunnerWatcher.ps1
	// when non-empty.
	RunnerDirectory string
	JitPath         string
	// Shutdown powers the VM off after a successful preparation.
	Shutdown bool
}

var prepareUserPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,20}$`)

const prepareGuestDir = `C:\Windows\Temp\scaleset-prepare`

// PrepareWindowsTemplate configures a running Windows VM through the guest
// agent so clones run the runner in an interactive desktop session: it creates
// the Administrator account, enables automatic logon and installs the watcher
// with -InteractiveUser. It does not convert the VM to a template.
func PrepareWindowsTemplate(ctx context.Context, cfg config.ProxmoxConfig, opts PrepareWindowsOptions, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if opts.VMID <= 0 {
		return fmt.Errorf("prepare windows: vmid is required")
	}
	if !prepareUserPattern.MatchString(opts.UserName) {
		return fmt.Errorf("prepare windows: user must be 1-20 characters of letters, digits, '_', '.' or '-'")
	}
	if opts.Password == "" {
		return fmt.Errorf("prepare windows: password is required")
	}
	p := &pmox{cfg: cfg, cli: newProxmoxClient(cfg), log: log}
	vm, err := p.findVM(ctx, opts.VMID)
	if err != nil {
		return err
	}
	return p.prepareWindows(ctx, vm, opts)
}

// findVM locates a running, non-template VM by VMID on any node.
func (p *pmox) findVM(ctx context.Context, vmid int) (*VM, error) {
	statuses, err := p.cli.Nodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare windows: list nodes: %w", err)
	}
	for _, ns := range statuses {
		node, err := p.cli.Node(ctx, ns.Node)
		if err != nil {
			continue
		}
		pVM, err := node.VirtualMachine(ctx, vmid)
		if err != nil {
			continue
		}
		if isTemplate(pVM) {
			return nil, fmt.Errorf("prepare windows: vmid %d is already a template; convert it back to a VM and start it first", vmid)
		}
		if pVM.Status != "running" {
			return nil, fmt.Errorf("prepare windows: vmid %d is %q; start it and wait for the guest agent", vmid, pVM.Status)
		}
		return &VM{VMID: vmid, Node: ns.Node, Name: pVM.Name}, nil
	}
	return nil, fmt.Errorf("prepare windows: vmid %d not found on any node", vmid)
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// psEncodedCommand builds a powershell.exe argv running script via -EncodedCommand.
func psEncodedCommand(script string) []string {
	units := utf16.Encode([]rune(script))
	b := make([]byte, len(units)*2)
	for i, u := range units {
		b[2*i] = byte(u & 0xff)
		b[2*i+1] = byte(u >> 8)
	}
	return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(b)}
}

func psFileCommand(file string, args ...string) []string {
	return append([]string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file}, args...)
}

func (p *pmox) prepareWindows(ctx context.Context, vm *VM, opts PrepareWindowsOptions) error {
	redact := func(err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), opts.Password, "***"))
	}
	step := func(name string, err error) error {
		if err != nil {
			return fmt.Errorf("prepare windows (%s): %w", name, redact(err))
		}
		p.log.Info("prepare windows step ok", "vmid", vm.VMID, "step", name)
		return nil
	}
	join := func(name string) string { return prepareGuestDir + `\` + name }

	// A directory limited to SYSTEM and Administrators holds the scripts and the password file.
	mkdir := "$ErrorActionPreference = 'Stop'; $d = " + psQuote(prepareGuestDir) + "; " +
		"if (Test-Path -LiteralPath $d) { Remove-Item -LiteralPath $d -Recurse -Force }; " +
		"New-Item -ItemType Directory -Path $d -Force | Out-Null; " +
		"$acl = New-Object System.Security.AccessControl.DirectorySecurity; $acl.SetAccessRuleProtection($true, $false); " +
		"foreach ($sid in @('S-1-5-18','S-1-5-32-544')) { $acl.AddAccessRule((New-Object System.Security.AccessControl.FileSystemAccessRule((New-Object System.Security.Principal.SecurityIdentifier($sid)), 'FullControl', 'ContainerInherit, ObjectInherit', 'None', 'Allow'))) }; " +
		"Set-Acl -LiteralPath $d -AclObject $acl"
	if err := step("create staging directory", p.agentExecWait(ctx, vm.Node, vm.VMID, psEncodedCommand(mkdir))); err != nil {
		return wrapGuestAgent(err)
	}
	// Always remove staged scripts and any leftover password file.
	defer func() {
		cleanup := "Remove-Item -LiteralPath " + psQuote(prepareGuestDir) + " -Recurse -Force -ErrorAction SilentlyContinue"
		if err := p.agentExecWait(context.WithoutCancel(ctx), vm.Node, vm.VMID, psEncodedCommand(cleanup)); err != nil {
			p.log.Warn("prepare windows: could not remove staging directory", "vmid", vm.VMID, "dir", prepareGuestDir)
		}
	}()

	for _, name := range []string{"Install-JitRunnerWatcher.ps1", "Watch-JitRunner.ps1", "Test-JitConfig.ps1", "Initialize-InteractiveRunner.ps1"} {
		data, err := fs.ReadFile(windowsscripts.Files, name)
		if err != nil {
			return fmt.Errorf("prepare windows: embedded script %s: %w", name, err)
		}
		if err := step("write "+name, p.agentFileWrite(ctx, vm.Node, vm.VMID, join(name), data)); err != nil {
			return err
		}
	}
	if err := step("write credentials", p.agentFileWrite(ctx, vm.Node, vm.VMID, join("password"), []byte(opts.Password))); err != nil {
		return err
	}
	if err := step("configure account and automatic logon", p.agentExecWait(ctx, vm.Node, vm.VMID,
		psFileCommand(join("Initialize-InteractiveRunner.ps1"), "-UserName", opts.UserName, "-PasswordFile", join("password")))); err != nil {
		return err
	}
	install := []string{"-InteractiveUser", `.\` + opts.UserName}
	if opts.RunnerDirectory != "" {
		install = append(install, "-RunnerDirectory", opts.RunnerDirectory)
	}
	if opts.JitPath != "" {
		install = append(install, "-JitPath", opts.JitPath)
	}
	if err := step("install interactive watcher", p.agentExecWait(ctx, vm.Node, vm.VMID, psFileCommand(join("Install-JitRunnerWatcher.ps1"), install...))); err != nil {
		return err
	}
	verify := "$ErrorActionPreference = 'Stop'; $t = Get-ScheduledTask -TaskName 'ActionsRunnerScaleSetWatcher'; " +
		"if ($t.Principal.LogonType -ne 'Interactive') { throw 'task is not interactive' }"
	if err := step("verify scheduled task", p.agentExecWait(ctx, vm.Node, vm.VMID, psEncodedCommand(verify))); err != nil {
		return err
	}
	if opts.Shutdown {
		if err := step("shut down", p.Stop(ctx, vm)); err != nil {
			return err
		}
	}
	return nil
}
