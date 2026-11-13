// scaleset is the orchestrator binary. It runs an actions/scaleset
// listener backed by Proxmox VMs as ephemeral GitHub Actions runners.
//
// Subcommands:
//
//	scaleset run [--config=path]
//	scaleset prepare-windows-template --vmid=N [--user=RunnerUI]
//	scaleset version
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/app"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/provisioner"
)

// version is overridden via -ldflags at build time.
var version = "dev"

func main() {
	root := &cobra.Command{
		Use:           "scaleset",
		Short:         "Run GitHub Actions jobs as ephemeral Proxmox VMs",
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	var configPath string

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run the orchestrator until SIGINT/SIGTERM.",
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return app.Run(ctx, app.Options{
				ConfigPath: configPath,
				Version:    version,
			})
		},
	}
	runCmd.Flags().StringVarP(&configPath, "config", "c", "config.yaml", "Path to config YAML.")

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the build version and exit.",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println(version)
		},
	}

	var (
		prepCfg, prepUser, prepPassEnv, prepPassFile, prepRunnerDir, prepJitPath string
		prepVMID                                                                 int
		prepNoShutdown                                                           bool
	)
	prepCmd := &cobra.Command{
		Use:   "prepare-windows-template",
		Short: "Configure a running Windows VM so clones run the runner in an interactive desktop session.",
		Long: "Creates a local Administrator account, enables automatic logon, and installs the runner watcher as an\n" +
			"at-logon task for that account, all through the QEMU guest agent. The VM must be running with the runner\n" +
			"installed; afterwards convert it to a template. The password is read from an environment variable or file,\n" +
			"never a flag, and is stored in the guest in plaintext (Winlogon DefaultPassword): use a dedicated account.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			password := os.Getenv(prepPassEnv)
			if prepPassFile != "" {
				b, err := os.ReadFile(prepPassFile)
				if err != nil {
					return fmt.Errorf("read password file: %w", err)
				}
				password = strings.TrimRight(string(b), "\r\n")
			}
			if password == "" {
				return fmt.Errorf("no password: set $%s or pass --password-file", prepPassEnv)
			}
			cfg, err := config.Load(prepCfg)
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			if err := provisioner.PrepareWindowsTemplate(ctx, cfg.Proxmox, provisioner.PrepareWindowsOptions{
				VMID: prepVMID, UserName: prepUser, Password: password,
				RunnerDirectory: prepRunnerDir, JitPath: prepJitPath, Shutdown: !prepNoShutdown,
			}, slog.Default()); err != nil {
				return err
			}
			fmt.Println("Windows VM prepared. Convert it to a template when it has shut down.")
			return nil
		},
	}
	prepCmd.Flags().StringVarP(&prepCfg, "config", "c", "config.yaml", "Path to config YAML (Proxmox connection settings).")
	prepCmd.Flags().IntVar(&prepVMID, "vmid", 0, "VMID of the running Windows VM to prepare.")
	prepCmd.Flags().StringVar(&prepUser, "user", "RunnerUI", "Local Administrator account to create/update and log on automatically.")
	prepCmd.Flags().StringVar(&prepPassEnv, "password-env", "RUNNER_UI_PASSWORD", "Environment variable holding the account password.")
	prepCmd.Flags().StringVar(&prepPassFile, "password-file", "", "File holding the account password (overrides --password-env).")
	prepCmd.Flags().StringVar(&prepRunnerDir, "runner-dir", "", "Runner directory in the guest (default C:\\actions-runner).")
	prepCmd.Flags().StringVar(&prepJitPath, "jit-path", "", "JIT handoff file in the guest (must match the profile's windows_jit_path).")
	prepCmd.Flags().BoolVar(&prepNoShutdown, "no-shutdown", false, "Leave the VM running afterwards.")
	_ = prepCmd.MarkFlagRequired("vmid")

	root.AddCommand(runCmd, prepCmd, versionCmd)

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "scaleset: %v\n", err)
		os.Exit(1)
	}
}
