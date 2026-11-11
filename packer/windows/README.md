# Packer template: Windows Server GitHub Actions runner

Builds a Proxmox template for [Windows runners](../../docs/windows-runners.md): unattended
Windows Server install on UEFI with VirtIO, the QEMU guest agent, the Actions runner
(unregistered) and the JIT watcher task from [`scripts/windows/`](../../scripts/windows/README.md).
It reuses those same scripts, so a Packer-built template and a hand-prepared one behave identically.

> Status: syntax-validated and rendered, but not yet run end to end on a Proxmox host. Expect to
> adjust ISO names, the driver folder and boot timing for your environment.

## Choose a session mode

| Mode | Set | Result |
| --- | --- | --- |
| SYSTEM, Session 0 (default) | nothing | Unattended; no desktop. Task runs as SYSTEM at startup. |
| Interactive desktop | `interactive_user`, `interactive_password` | Creates that local Administrator, enables auto-logon, task starts at its logon in its desktop session (Session 1). Needs a Desktop Experience image. |

The interactive password is stored in plaintext in the template's Winlogon registry key because
Windows auto-logon requires it. Use a dedicated account with no reusable credentials.

## Prerequisites

1. Packer >= 1.10 (`packer init .` installs the Proxmox plugin).
2. ISOs: nothing needs uploading by default. The Proxmox host downloads the
   [virtio-win ISO](https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/)
   from Fedora and, if you set `windows_iso_url`, a Windows Server ISO. Microsoft's Server 2022
   *evaluation* ISO is freely downloadable without a login (the link is in the `.example` file; it is
   a 180-day evaluation, so don't use it for long-lived production runners). Licensed ISOs normally
   need a Microsoft account to download: upload yours to a Proxmox ISO store and set `windows_iso_file`.
   `iso_download_pve = false` routes downloads through the Packer host instead. Set
   `windows_iso_checksum`/`virtio_iso_checksum` for reproducible builds (the upstream ISOs are
   republished over time, so none is pinned here).
3. A Proxmox API token with the same permissions as the [Ubuntu build](../README.md).
4. `windows_image_name` must match an image name inside the ISO's `install.wim`
   (`... SERVERSTANDARD` = Desktop Experience, `... SERVERSTANDARDCORE` = Server Core), and
   `windows_driver_folder` the VirtIO folder for your Windows version (`2k22`, `2k25`, ...).

## Build

```sh
cp variables.auto.pkrvars.hcl.example variables.auto.pkrvars.hcl && $EDITOR variables.auto.pkrvars.hcl
export PKR_VAR_proxmox_token='...'
export PKR_VAR_interactive_password='...'   # only for interactive mode
packer init . && packer validate . && packer build .
```

Expect roughly 30-60 minutes. The result is a template at `template_vm_id` (default 9100); point a
profile's `template_vmid` at it and set `guest_os: windows`.

## How it works

1. `Autounattend.xml` (rendered from `answer_files/`, delivered on a generated CD) partitions the
   disk, loads VirtIO storage/network drivers, installs Windows and creates a temporary build admin.
2. At first logon `scripts/bootstrap.ps1` installs the VirtIO driver bundle and guest agent from the
   VirtIO ISO and enables WinRM, so Packer can find the VM's IP via the agent and connect.
3. Packer uploads the repo's Windows scripts, installs the runner (`install-runner.ps1`, with optional
   `runner_sha256` verification) and runs `Install-JitRunnerWatcher.ps1`, or, in interactive mode,
   `Initialize-InteractiveRunner.ps1` first.
4. `scripts/finalize.ps1` removes WinRM access and the build firewall rule, disables the build
   account, and fails the build if any JIT/`started` state exists. Packer then shuts down and templates the VM.

## Notes

- WinRM during the build uses unencrypted Basic auth on the build network. Build on a trusted
  network; the access is removed before templating.
- No Windows Update, Sysprep or hostname change is done. Clones share the template's computer
  name; add Sysprep or patching provisioners if your environment needs them.
- Windows 10/11 guests need extra VM settings (TPM 2.0) not configured here; Server is the supported path.
- Build repeatedly rather than editing a template that still has linked clones.
