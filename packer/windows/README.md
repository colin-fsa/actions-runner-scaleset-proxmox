# Packer template: Windows GitHub Actions runner (Windows 11 Pro by default)

Builds a Proxmox template for [Windows runners](../../docs/windows-runners.md): unattended
Windows install (Windows 11 Pro by default; Windows Server also works) on UEFI with a virtual TPM and VirtIO, the QEMU guest agent, the Actions runner
(unregistered) and the JIT watcher task from [`scripts/windows/`](../../scripts/windows/README.md).
It reuses those same scripts, so a Packer-built template and a hand-prepared one behave identically.

> Status: syntax-validated and rendered, but not yet run end to end on a Proxmox host. Expect to
> adjust ISO names, the driver folder and boot timing for your environment.

## Choose a session mode

| Mode | Set | Result |
| --- | --- | --- |
| SYSTEM, Session 0 (default) | nothing | Unattended; no desktop. Task runs as SYSTEM at startup. |
| Interactive desktop | `interactive_user`, `interactive_password` | Creates that local Administrator, enables auto-logon, task starts at its logon in its desktop session (Session 1). Needs a Desktop Experience edition (Windows 11 Pro qualifies). |

The interactive password is stored in plaintext in the template's Winlogon registry key because
Windows auto-logon requires it. Use a dedicated account with no reusable credentials.

## Prerequisites

1. Packer >= 1.10 (`packer init .` installs the Proxmox plugin).
2. ISOs. The Proxmox host downloads the
   [virtio-win ISO](https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/)
   from Fedora automatically. For Windows, either:
   - upload an ISO to a Proxmox ISO store and set `windows_iso_file` (best for repeat builds), or
   - set `windows_iso_url`. Microsoft's Windows 11 download links are generated per request on the
     [download page](https://www.microsoft.com/software-download/windows11) and expire after about
     24 hours, so this suits an immediate build. The Windows Server 2022 *evaluation* ISO has a stable,
     login-free link (in the `.example` file) but is a 180-day evaluation.
   `iso_download_pve = false` routes URL downloads through the Packer host instead. Set
   `windows_iso_checksum`/`virtio_iso_checksum` for reproducible builds (none is pinned here).
3. A Proxmox API token with the same permissions as the [Ubuntu build](../README.md).
4. `windows_image_name` must match an image name inside the ISO's `install.wim`
   (`Windows 11 Pro`; for Server, `... SERVERSTANDARD` is Desktop Experience and
   `... SERVERSTANDARDCORE` is Server Core), and `windows_driver_folder` the VirtIO folder for your
   Windows version (`w11`, `2k22`, `2k25`, ...).
5. Licensing: the template installs without a product key and runs unactivated. Activate with your own
   key or volume-activation setup (`windows_product_key` is only passed to Setup), and confirm that
   your Windows license permits running it in VMs for CI. Windows client editions have different
   virtualization terms from Windows Server.

## Build

```sh
cp variables.auto.pkrvars.hcl.example variables.auto.pkrvars.hcl && $EDITOR variables.auto.pkrvars.hcl
export PKR_VAR_proxmox_token='...'
export PKR_VAR_interactive_password='...'   # only for interactive mode
packer init . && packer validate . && packer build .
```

Expect roughly 30-60 minutes. It has not been run end to end on Windows 11 yet. The result is a template at `template_vm_id` (default 9100); point a
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
- A virtual TPM 2.0 is attached by default (`enable_tpm`) for Windows 11. Windows 11 setup occasionally
  changes its OOBE prompts between releases; if a build stalls at a screen, watch the VM console and
  adjust `answer_files/Autounattend.xml.pkrtpl.hcl`.
- Build repeatedly rather than editing a template that still has linked clones.
