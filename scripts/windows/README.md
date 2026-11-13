# Windows runner image preparation

Install the GitHub Actions runner at `C:\actions-runner` and QEMU guest agent in a Windows VM first. From an elevated PowerShell session, run `./Install-JitRunnerWatcher.ps1` beside `Watch-JitRunner.ps1` and `Test-JitConfig.ps1`. Use `-RunnerDirectory` if the runner is installed elsewhere. `-JitPath` can select a different filename within `C:\ProgramData\ActionsRunnerScaleSet`; it must match the profile’s `windows_jit_path`. Other directories require a custom watcher and appropriately secured ACLs. This installs an at-startup task running as SYSTEM in Session 0 and secures `C:\ProgramData\ActionsRunnerScaleSet` for SYSTEM and Administrators. The runner and task must be installed before imaging; the boot watcher does not install or update either.

A previously configured runner must first be deregistered in GitHub with a GitHub runner removal token. Then pass `-RemoveExistingRegistration` to explicitly stop and delete its local service and remove its local registration credentials. This switch does not perform remote deregistration. Do not use it to reset a used clone. Preparation refuses a preexisting `jitconfig` or `started` marker; clean those only deliberately on the golden image, before cloning.

The controller atomically publishes a raw base64 JIT config to `C:\ProgramData\ActionsRunnerScaleSet\jitconfig`. The watcher waits for it, creates a persistent `started` marker before launching `run.cmd --jitconfig`, waits for exit, then shuts down, forcing any remaining applications to close. This is intended for disposable dedicated runner VMs. The handoff remains available for controller recovery. A marker on a subsequent boot prevents another runner launch and causes shutdown. A failed launch is not retried. Runner output is suppressed because it can contain secrets. SYSTEM and administrators can still read the handoff and local process arguments; protect access to the VM accordingly.

## Optional: interactive desktop session

By default the watcher runs as SYSTEM in Session 0. For workloads that need a real desktop (GUI/UI tests, desktop automation), pass `-InteractiveUser '.\RunnerUI'` to register the task instead as an at-logon task for that account with an interactive logon type, so the runner inherits the user's desktop session (typically Session 1; Windows does not guarantee the number). Requirements:

- The account must be a local Administrator (the installer checks). The task runs elevated and the state directory ACL remains SYSTEM + Administrators only; no ACL is loosened.
- Configure automatic logon for that account on the golden image (for example Sysinternals Autologon or `netplwiz`) so the task starts on every clone boot. Without it the watcher never starts. The installer does not handle the password. Autologon stores credentials on the image; use a dedicated, low-privilege-network account with no reusable secrets.
- Disable the lock screen/screensaver timeout and any first-run prompts for that account so the session stays usable.

Default SYSTEM mode is unchanged. To switch modes, re-run the installer on a clean golden image; the task is re-registered with `-Force`.

### Automated preparation

Instead of doing the account, auto-logon and installer steps by hand, the controller can do them through the QEMU guest agent. Install Windows, the runner at `C:\actions-runner` and the guest agent, leave the VM running (not yet a template), then:

```
RUNNER_UI_PASSWORD='...' scaleset prepare-windows-template --config config.yaml --vmid 120 --user RunnerUI
```

It creates or updates the local Administrator account, writes the Winlogon auto-logon values, disables the lock screen and display/standby timeouts, runs `Install-JitRunnerWatcher.ps1 -InteractiveUser .\RunnerUI`, verifies the task is interactive, removes its staging files and powers the VM off (`--no-shutdown` to skip). Then convert the VM to a template. Options: `--password-file`, `--password-env`, `--runner-dir`, `--jit-path` (must match `windows_jit_path`).

The password is read from an environment variable or file, never a flag, and is never put on a guest command line; it travels in a temporary file in a SYSTEM/Administrators-only directory that is deleted after use. It does remain in the guest as the Winlogon `DefaultPassword` registry value (plaintext) because Windows needs it for auto-logon: use a dedicated account with no reusable credentials. The Proxmox API token needs guest-agent permissions (`VM.GuestAgent.FileWrite`/`Unrestricted` or equivalent for exec) on the VM.

