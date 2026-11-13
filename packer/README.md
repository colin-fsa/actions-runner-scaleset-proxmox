# Packer templates: GitHub Actions runners (Ubuntu 26.04 and Windows Server)

This directory builds two templates, selected with `-only`:

```sh
packer build -only='ubuntu-2604-runner.*' .   # Linux (below)
packer build -only='windows-runner.*' .       # Windows (see the Windows section)
packer build .                                # both
```

Connection, storage and network variables are shared between the builds.


Builds a Proxmox VM template that the [orchestrator](../README.md) clones into
ephemeral GitHub Actions runners.

## What the template contains

- Ubuntu 26.04 LTS server, installed via Subiquity autoinstall.
- `qemu-guest-agent` enabled — the orchestrator drives the VM through this.
- GitHub Actions runner unpacked at `/opt/actions-runner/`.
- A systemd path-unit (`gh-runner.path`) that watches for
  `/opt/actions-runner/jitconfig`. When the orchestrator writes that file via
  guest-agent file-write, the corresponding `gh-runner.service` starts the
  runner with `--jitconfig "$(cat …)"`. The runner takes one job and exits;
  the service's `ExecStopPost=systemctl poweroff` then shuts the VM down so
  the orchestrator's `Destroy` path doesn't have to wait.
- Kernel + service hardening (see [files/99-scaleset-hardening.conf](files/99-scaleset-hardening.conf)
  and [scripts/provision.sh](scripts/provision.sh)).

## Hardening posture

| Surface | Decision |
| --- | --- |
| SSH | **Removed** — `openssh-server` is purged. Operator access via Proxmox console or `qm guest exec`. |
| Build user | Locked + removed from `sudo` group as the final provisioning step. |
| Runner user | Non-root (`runner`), no password, NOPASSWD sudo restricted to that account. |
| Runner service sandbox | **None** — the isolation boundary is the single-use VM. systemd sandboxing (`NoNewPrivileges`, `ProtectSystem`, `Protect*Kernel*`) breaks `sudo` and ordinary CI toolchain installers for no real containment gain. |
| Network firewall | Off (ephemeral VMs; firewall is enforced at the Proxmox/network level). |
| Kernel sysctls | rp_filter, no redirects, syncookies, kptr_restrict=2, dmesg_restrict, BPF lockdown, protected hardlinks/symlinks/fifos/regular. |
| Snap / unattended-upgrades / iscsi / multipath / ModemManager | Removed or masked. |
| Swap | Disabled and removed from fstab. |
| Core dumps | Disabled (limits.conf + suid_dumpable=0). |
| machine-id | Cleared so clones generate a unique ID on first boot. |
| Secure Boot | UEFI enabled, pre-enrolled keys **off** by default (toggle per-deployment after testing your kernel). |

## One-time setup

1. **Install Packer >= 1.10** and the Proxmox plugin:

    ```sh
    packer init .
    ```

2. **Create a Proxmox API token** with `VM.Allocate`, `VM.Audit`, `VM.Clone`,
   `VM.Config.*`, `VM.PowerMgmt`, `Datastore.AllocateSpace`,
   `Datastore.AllocateTemplate`, and `Datastore.Audit` on `/`.

3. **Download the Ubuntu 26.04 LTS server ISO checksum** from
   <https://releases.ubuntu.com/26.04/> and copy your local values into:

    ```sh
    cp variables.auto.pkrvars.hcl.example variables.auto.pkrvars.hcl
    $EDITOR variables.auto.pkrvars.hcl
    ```

4. **Export the API token secret** so it doesn't end up in the var-file:

    ```sh
    export PKR_VAR_proxmox_token='your-token-secret'
    ```

## Build

```sh
packer validate .
packer build .
```

Build time is typically 10–20 minutes (Ubuntu autoinstall + apt-get + runner
tarball download).

When the build finishes you'll have a Proxmox template at the VMID you
configured (`template_vm_id`, default 9000). Point the orchestrator's
`proxmox.template_vmid` at it.

## Re-building

The template is meant to be rebuilt regularly (monthly is reasonable) so that
security updates land — ephemeral VMs deliberately have no
`unattended-upgrades` and pull no updates at boot. Bump `runner_version` in
`variables.auto.pkrvars.hcl` whenever a new Actions runner is published.

## Notes

- The Subiquity password hash in [http/user-data](http/user-data) corresponds to
  the literal string `ubuntu`. It's only valid during the Packer build window;
  the build user is locked before the template is finalized.
- `gh-runner.service` (in [files/](files/)) runs the runner unsandboxed, the
  same posture as a GitHub-hosted runner. Add systemd hardening back only if
  you know your workflows never need `sudo`, kernel modules, cgroup writes, or
  writes outside the runner's own tree.
- If you need a `arm64` template, set `runner_arch = "arm64"` and switch the
  ISO URL/checksum to the arm64 release; ensure your Proxmox node supports
  arm64 emulation (or runs on arm64 hardware).

## Shared cache (Linux)

By default the template mounts the host directory `/srv/shared/linux-runners-shared` (the same path
the LXC runners use) over virtiofs. The runner user is created with uid/gid `runner_uid` (default
101000, the host id of the LXC runners' uid 1000), because virtiofs passes ids through unmapped and
the existing caches are owned by that id. pnpm's store is SQLite and virtiofs locks are not coherent
between guests, so `<mount>/pnpm-store` is bind-mounted from a per-runner local directory
(`/var/lib/pnpm-store`); pip and ESLint caches stay shared. Set
`shared_cache_mount=""` to disable. Packer cannot attach virtiofs, so after the build:

```
pvesh create /cluster/mapping/dir --id linux-runners-shared --map node=<node>,path=/srv/shared/linux-runners-shared  # once
qm set <template_vm_id> --virtiofs0 linux-runners-shared
```

Clones inherit the device and mount it at boot (`nofail`, so they still boot without it).

## Windows template

`proxmox-windows.pkr.hcl` builds a Windows Server 2022 template using the same scripts as
[manual preparation](../scripts/windows/README.md), so a Packer-built and a hand-prepared template
behave identically. Windows-specific variables are in `variables-windows.pkr.hcl`; the unattended
answer file and first-logon scripts are in [`windows/`](windows/).

> Status: not yet validated against every Windows release. Adjust ISO, driver folder and
> image name for your environment.

Nothing needs uploading by default: the Proxmox host downloads Microsoft's free Server 2022
*evaluation* ISO (a 180-day evaluation; no login) and the
[virtio-win ISO](https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/).
For a licensed ISO, upload it and set `windows_iso_file`. Downloading by URL needs the same Proxmox
permissions as the Ubuntu build; set `iso_download_pve = false` to download through the Packer host.
Pin `windows_iso_checksum`/`virtio_iso_checksum` for reproducible builds.

### How it works

1. `Autounattend.xml` partitions the disk, loads VirtIO storage/network drivers, installs Windows
   and creates a temporary build admin. `bootstrap.ps1` then installs the VirtIO drivers and QEMU
   guest agent and enables WinRM (Packer finds the VM's address through the guest agent).
2. Packer uploads the repo's Windows scripts, installs the runner (`install-runner.ps1`, optional
   `runner_sha256` verification) and fully patches Windows (community plugin
   [`rgl/windows-update`](https://github.com/rgl/packer-plugin-windows-update), fetched by
   `packer init`).
3. `Install-JitRunnerWatcher.ps1` registers the watcher task: SYSTEM in Session 0 by default, or,
   with `interactive_user`/`interactive_password`, an Administrator with automatic logon so the
   runner gets a desktop session (`Initialize-InteractiveRunner.ps1`). The password is stored in
   plaintext in the template (Windows auto-logon requires it): use a dedicated account.
4. `finalize.ps1` removes WinRM access and the build account and fails if any JIT state exists.
   Packer then shuts down and templates the VM.

Like the Linux build, SSH-style build access is removed at the end (here WinRM, which is used instead
of SSH because Packer's reboot handling for Windows Update needs it). No Sysprep is run and clones
share the template's computer name. Point a `guest_os: windows` profile's `template_vmid` at
`windows_template_vm_id` (default 9100).
