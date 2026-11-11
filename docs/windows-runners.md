# Windows runners

A Windows profile uses a prepared Windows VM template and the QEMU guest agent
for the same single-use JIT registration as Linux. Put Windows and Linux in
separate scale sets with different labels: GitHub routes to a scale set, not an
individual profile. All profiles in a scale set must use the same guest OS and
advertise its labels. See [the complete mixed example](../examples/windows-linux.yaml).

## Build the template

There are three ways to get a Windows template. All of them end in the same
result: Windows, the QEMU guest agent, an unregistered runner at
`C:\actions-runner`, and the `ActionsRunnerScaleSetWatcher` scheduled task.

| Path | Use when |
| --- | --- |
| [A. Packer](#a-packer-recommended) | You want a repeatable, from-scratch build. |
| [B. Existing VM + `prepare-windows-template`](#b-existing-vm-and-the-prepare-command) | You already have a Windows VM with the runner and want the controller to finish setup. |
| [C. By hand](#c-by-hand) | You want full control, or are customizing the scripts. |

Put the finished template's VMID in a profile with `guest_os: windows` (see
[Configure and route jobs](#configure-and-route-jobs)).

### Choose a session mode first

| Mode | Behavior | Choose it for |
| --- | --- | --- |
| **SYSTEM / Session 0** (default) | Task runs as Local System at startup. No desktop; jobs cannot rely on an RDP profile, mapped drives or GUI prompts. | Builds, tests and scripts that run unattended. |
| **Interactive desktop** | Windows signs in automatically as a local Administrator; the task starts at that logon and the runner inherits the desktop session (typically Session 1). | GUI/UI tests, desktop automation, anything that needs a real desktop. |

Interactive mode needs a Desktop Experience image, an Administrator account and
automatic logon. The auto-logon password is stored in plaintext in the template
(`Winlogon\DefaultPassword`) because Windows requires it; the runner itself
never uses it. Use a dedicated account with no reusable credentials, and note
that jobs running as that Administrator can read the value. The tooling below
handles all of this for you when you give it an interactive user.

### A. Packer (recommended)

The Windows build in [`packer/`](../packer/README.md#windows-template) uses the same
variables and Proxmox credentials as the Linux build, so the existing setup works
unchanged: `packer build -only='windows-runner.*' .` (or no `-only` to build both). It installs
Windows Server unattended, fully patches it, adds VirtIO drivers and the guest agent, installs the
runner and the watcher, and removes its own build-time access. Set `interactive_user` and
`interactive_password` for interactive mode. Nothing needs uploading: Proxmox downloads
Microsoft's free Server 2022 evaluation ISO (180-day evaluation) and the virtio-win ISO; for a
licensed ISO, upload it and set `windows_iso_file`.

### B. Existing VM and the prepare command

If you already have a running Windows VM (not yet a template) with the runner
under `C:\actions-runner` and the guest agent working, the controller can finish
the job through the guest agent:

```sh
RUNNER_UI_PASSWORD='...' scaleset prepare-windows-template \
  --config config.yaml --vmid 120 --user RunnerUI
```

It creates or updates the Administrator account, configures automatic logon and
disables the lock screen and display/standby timeouts, installs the watcher in
interactive mode, verifies the task, removes its staging files and shuts the VM
down. See [scripts/windows/README.md](../scripts/windows/README.md) for
options. The Proxmox token needs guest-agent file-write and exec permission on
that VM. The password is read from an environment variable or file, never a
flag, and is never placed on a guest command line.

A template cannot be started, so to use this on an existing template first make
a startable VM: clone it to a new full VM (leaving the original untouched), or
convert it back with `qm set <vmid> --template 0` (not possible while linked
clones depend on it). Take a Proxmox snapshot before running the command, start
the VM, wait for the guest agent, run the command, then convert the result back
with `qm template <vmid>` and point your profile's `template_vmid` at it. The
installer refuses a VM that already has a `jitconfig` or `started` marker, so
always start from a clean VM, never from a clone that has run a job.

### C. By hand

1. Install Windows and updates, the build tools your jobs need, VirtIO drivers
   as appropriate, and the GitHub Actions runner distribution under
   `C:\actions-runner`. Do not register the runner in the template. Verify the
   VM can reach GitHub and any required package and artifact endpoints. Account
   for Windows licensing and activation in your environment.
2. Install the QEMU guest agent in Windows, enable its service, and enable the
   QEMU guest agent in the Proxmox VM options. Verify the agent responds from
   Proxmox after a reboot. The controller uses it to publish the JIT file; SSH
   is not required.
3. If this VM already has a conventional GitHub runner, deregister it in
   GitHub with its removal token first. Follow the safeguards and installation
   commands in [scripts/windows/README.md](../scripts/windows/README.md): run
   `Install-JitRunnerWatcher.ps1` elevated, with its cleanup switch only when
   appropriate. The installer refuses existing registration state by default.
   Ensure no runner credentials, pending JIT file, started marker, job data,
   or other secrets are in the golden image.
4. For SYSTEM mode, the installer creates a boot-triggered task running as
   Local System (Session 0). For interactive mode, create a local Administrator,
   configure automatic logon for it (Sysinternals Autologon or `netplwiz`),
   disable its lock screen and screensaver, and run the installer with
   `-InteractiveUser '.\RunnerUI'`.
5. Shut down the clean VM and convert it to a Proxmox template. Keep the
   template unchanged while linked clones depend on it; build a new template
   for updates and retire the old one only after its clones are gone.

The shipped watcher reads
`C:\ProgramData\ActionsRunnerScaleSet\jitconfig`. `guest_os: windows` selects
Windows injection; `windows_jit_path` can override the destination, but it must
match the guest launcher. The installer accepts `-RunnerDirectory` and
`-JitPath` (a filename within its secured handoff directory); other directories
require a custom launcher and appropriate ACLs. The
file is a sensitive, single-use handoff; do not print or copy it into a golden
image. The watcher starts the runner once and shuts down the VM after it exits.
See the script README for behavior and troubleshooting. Sysprep is not run on
each clone. Decide whether to generalize the golden image once based on your
Windows deployment, identity, domain and licensing requirements; validate the
result before conversion.

## Configure and route jobs

Copy [examples/windows-linux.yaml](../examples/windows-linux.yaml), replace the
placeholder org, node, endpoint, token ID, storage, bridge and template VMIDs,
and provide `SCALESET_GITHUB_PAT_TOKEN` and
`SCALESET_PROXMOX_AUTH_TOKEN_SECRET` securely. Store the deployed config with
restrictive permissions (for example, `chmod 600 config.yaml`). The profile defaults to Linux
when `guest_os` is omitted; Windows explicitly sets `guest_os: windows`.
Ranges must be disjoint and free of existing VMIDs. Both templates must be
available to the selected node and their actual storage must support the
requested clone and snapshot operations. The example starts with zero hot and
warm capacity for both profiles, so initial demand takes the cold clone and
boot path. Adjust concurrency and capacity only after measuring your hardware.

Select the intended scale set in a workflow, for example:

```yaml
jobs:
  windows:
    runs-on: [self-hosted, windows, x64, proxmox-windows]
    steps:
      - run: Write-Output 'Windows runner'
  linux:
    runs-on: [self-hosted, linux, x64, proxmox-linux]
    steps:
      - run: echo 'Linux runner'
```

Use OS-specific labels in every job; a job requesting only shared labels may
match either scale set. Restrict runner groups and repository access as needed.

## Storage, recycling and performance

The example requests linked clones and `snapshot_rollback` globally. Proxmox's
[LVM-thin documentation](https://pve.proxmox.com/pve-docs/pve-admin-guide.html#storage_lvmthin)
describes local LVM-thin storage with linked clones and snapshots. Linked
clones stay on the template's node and storage; a storage name shared across
nodes does not make local LVM-thin data shared. All VM disks and devices must
support the requested snapshot operation. The recycler snapshots each stopped
clone before first boot, then rolls back after a job; if snapshot creation or
rollback fails, it falls back to destroy and replacement. `vm_max_age` still
replaces old clones to pick up updated templates. See
[recycling](profiles-and-scaling.md#recycling-vms-with-snapshot-rollback).

The controller clones the template's current state; selecting a named source
snapshot is not a configuration option. A snapshot is created on the clone for
recycling, not selected as a clone source. Verify that linked clone plus
snapshot rollback works on your exact Proxmox version, storage and VM devices
before relying on it. Monitor thin-pool data **and metadata** free capacity,
copy-on-write growth, RAM, CPU and I/O; thin provisioning does not create
physical capacity. Hot VMs consume resources continuously; warm VMs avoid
clone latency but still need a boot. A zero/zero pool trades idle cost for cold
start latency, and recycling effectiveness at that size should be measured.

## Manual canary validation

Use an isolated test repository and small concurrency limit. Record the
Proxmox version, storage, template IDs, guest versions, VMIDs and timestamps.

1. Verify both scale sets register with the intended labels and only intended
   repositories can use them. Submit one harmless job for each OS and verify
   OS identity, guest-agent readiness, successful completion and VM shutdown.
2. For Windows, verify the scheduled task runs as SYSTEM (or the configured interactive user, and
   that the runner's session is the interactive one) and the runner accepts
   exactly one job. Confirm the baseline has no registration or JIT state;
   avoid exposing the JIT file or credentials in logs and artifacts.
3. Inspect clone, snapshot and rollback tasks and controller logs/metrics. Run
   a second Windows job after creating a harmless sentinel in the first and
   verify it is absent; confirm a fresh JIT registration is used. Observe
   whether the VM was actually recycled or destroyed, including fallback.
4. Record cold, warm and hot queue-to-start times separately if those tiers are
   configured, along with clone, boot and recycle timings and host/thin-pool
   peaks. The startup stage metric in [operations](operations.md) helps break
   down power-on, guest-agent, injection and online waits. Test a representative Windows build, not only an empty job. Size
   pools and reserve headroom from those measurements; repeat after template,
   storage or workload changes.

These are validation steps, not a claim that your environment has been tested.

## Recovery and operational limits

After a restart, Windows guests with a published JIT handoff or an unreadable
handoff are held as assigned, rather than offered a second job. A missing file
may cause this conservative classification too; the normal registration grace
and cleanup recover it. Stopped Windows guests are rolled back in snapshot
mode or destroyed in destroy mode before reuse. Unknown profiles are destroyed.

For linked-clone canary rollouts, keep the stable and canary templates on the
same node. Candidate selection currently happens after node admission; a
candidate on another node fails closed and is cleaned up. Named source snapshot
selection is deferred: prepared stopped templates already supply the supported
linked-clone baseline without introducing a second source-state lifecycle.


### Failed startup and cleanup

New disposable clones explicitly clear inherited Proxmox deletion protection;
the source template stays protected. The controller waits for Proxmox's
asynchronous configuration task (tags and hardware included) before booting.
If any VM in a profile is draining, destroying or poisoned, new cloning pauses
until cleanup completes. This prevents a persistent startup failure and failed
cleanup from accumulating replacement VMs.

A JIT temporary-write failure occurs before the runner receives its config.
Verify template preparation from an elevated PowerShell console on one clone:

```powershell
Test-Path 'C:\ProgramData\ActionsRunnerScaleSet'
Get-ScheduledTask -TaskName ActionsRunnerScaleSetWatcher -ErrorAction SilentlyContinue |
    Select-Object TaskName, State, Actions
Test-Path 'C:\actions-runner\run.cmd'
Get-Service -Name QEMU-GA -ErrorAction SilentlyContinue | Select-Object Name, Status
```

The directory, watcher task and runner must exist, and the QEMU guest agent must
be running. Missing components require one-time template preparation above;
increasing boot timeouts does not install them. Do not relaunch the whole fleet
to diagnose a template problem. Inspect one clone while the controller is stopped.
Already-created protected clones are not automatically unprotected by this fix;
review their identities before separately approving cleanup.
