# Windows Server 2022 runner template for the orchestrator.
#
# Produces a Proxmox template containing:
#   - Windows installed unattended on UEFI with VirtIO storage/network.
#   - The QEMU guest agent (required for JIT injection) and VirtIO drivers.
#   - The GitHub Actions runner at C:\actions-runner (unregistered).
#   - The ActionsRunnerScaleSetWatcher scheduled task (scripts/windows/), either as SYSTEM in
#     Session 0 (default) or, when var.interactive_user is set, as that Administrator in an
#     interactive desktop session with automatic logon.
#
# Packer drives the guest over WinRM during the build only; finalize.ps1 removes that access.
#
# Build only this template (the Ubuntu build is in proxmox-ubuntu.pkr.hcl):
#   packer init .
#   packer build -only='windows-runner.*' .

packer {
  required_plugins {
    # Community plugin: installs Windows Updates over WinRM and survives the reboots they need.
    # (The proxmox plugin is declared in proxmox-ubuntu.pkr.hcl.)
    windows-update = {
      version = ">= 0.16.0"
      source  = "github.com/rgl/windows-update"
    }
  }
}

source "proxmox-iso" "windows_runner" {
  proxmox_url              = var.proxmox_url
  username                 = var.proxmox_username
  token                    = var.proxmox_token
  insecure_skip_tls_verify = var.proxmox_insecure_skip_tls_verify
  node                     = var.proxmox_node

  os       = "win11" # Proxmox OS type for Windows 11 / Server 2022+ guests
  cpu_type = "host"
  cores    = var.windows_vm_cores
  sockets  = 1
  memory   = var.windows_vm_memory_mb
  machine  = "q35"
  bios     = "ovmf"

  # VirtIO GPU: the default VGA leaves Windows on the Basic Display Adapter, which cannot change resolution
  # (SetDisplayConfig reports success but the mode never applies). viogpudo is already in the driver store.
  vga {
    type = "virtio"
  }
  efi_config {
    efi_storage_pool  = var.storage_pool
    efi_type          = "4m"
    pre_enrolled_keys = true # Windows Setup expects Microsoft's keys enrolled
  }

  scsi_controller = "virtio-scsi-single"
  boot            = "order=scsi0;ide2;net0"

  disks {
    type         = "scsi"
    storage_pool = var.storage_pool
    disk_size    = "${var.windows_vm_disk_size_gb}G"
    format       = "raw"
    io_thread    = true
    discard      = true
    ssd          = true
    # Builds are disposable until converted; Windows Update servicing is sync-write bound.
    cache_mode = "unsafe"
  }

  network_adapters {
    bridge   = var.network_bridge
    model    = "virtio"
    vlan_tag = var.network_vlan_tag != 0 ? "${var.network_vlan_tag}" : ""
    firewall = false
  }

  qemu_agent = true

  # Windows install media.
  boot_iso {
    type             = "ide"
    index            = "2"
    iso_url          = var.windows_iso_file == "" ? var.windows_iso_url : ""
    iso_file         = var.windows_iso_file
    iso_checksum     = var.windows_iso_checksum
    iso_storage_pool = var.iso_storage_pool
    iso_download_pve = var.windows_iso_file == "" ? var.iso_download_pve : false
    unmount          = true
  }

  # VirtIO drivers + guest agent installers.
  additional_iso_files {
    type             = "ide"
    index            = "3"
    iso_url          = var.virtio_iso_file == "" ? var.virtio_iso_url : ""
    iso_file         = var.virtio_iso_file
    iso_checksum     = var.virtio_iso_checksum
    iso_storage_pool = var.iso_storage_pool
    iso_download_pve = var.virtio_iso_file == "" ? var.iso_download_pve : false
    unmount          = true
  }

  # Autounattend.xml and the first-logon bootstrap script ride on a small generated CD.
  additional_iso_files {
    type             = "sata"
    index            = "0"
    iso_storage_pool = var.iso_storage_pool
    cd_label         = "UNATTEND"
    cd_content = {
      "Autounattend.xml" = templatefile("${path.root}/windows/Autounattend.xml.pkrtpl.hcl", {
        image_name     = var.windows_image_name
        driver_folder  = var.windows_driver_folder
        product_key    = var.windows_product_key
        build_username = var.windows_build_username
        build_password = var.windows_build_password
      })
      "bootstrap.ps1" = file("${path.root}/windows/bootstrap.ps1")
    }
    unmount = true
  }

  # UEFI media prompts "Press any key to boot from CD"; tap a key at the right moment.
  boot_wait    = "3s"
  boot_command = ["<spacebar><wait2><spacebar><wait2><spacebar><wait2><spacebar>"]

  # WinRM (build-time only). The address comes from the QEMU guest agent.
  communicator   = "winrm"
  winrm_username = var.windows_build_username
  winrm_password = var.windows_build_password
  winrm_use_ssl  = false
  winrm_insecure = true
  winrm_timeout  = "2h"

  tags = "windows;gh-runner;packer"
}

# Runner layer: a full clone of the patched base template (var.windows_base_vm_id), so only the
# runner-specific steps run on each rebuild. Full (not linked) so the base can be replaced monthly.
source "proxmox-clone" "windows_layer" {
  proxmox_url              = var.proxmox_url
  username                 = var.proxmox_username
  token                    = var.proxmox_token
  insecure_skip_tls_verify = var.proxmox_insecure_skip_tls_verify
  node                     = var.proxmox_node

  clone_vm_id          = var.windows_base_vm_id
  full_clone           = true
  vm_id                = var.windows_template_vm_id
  vm_name              = "${var.windows_template_name}-builder"
  template_name        = var.windows_template_name
  template_description = "Windows GitHub Actions runner (${var.interactive_user != "" ? "interactive" : "SYSTEM/Session 0"}), layered on base ${var.windows_base_vm_id}. Built {{ timestamp }}."

  os       = "win11"
  cpu_type = "host"
  cores    = var.windows_vm_cores
  sockets  = 1
  memory   = var.windows_vm_memory_mb

  scsi_controller = "virtio-scsi-single"
  network_adapters {
    bridge   = var.network_bridge
    model    = "virtio"
    vlan_tag = var.network_vlan_tag != 0 ? "${var.network_vlan_tag}" : ""
    firewall = false
  }
  qemu_agent = true

  communicator   = "winrm"
  winrm_username = var.windows_build_username
  winrm_password = var.windows_build_password
  winrm_use_ssl  = false
  winrm_insecure = true
  winrm_timeout  = "30m"

  tags = "windows;gh-runner;packer"
}

build {
  name = "windows-runner"

  # Full build from ISO (install, patch, runner) -- the original single-stage path.
  source "source.proxmox-iso.windows_runner" {
    name                 = "windows_runner"
    vm_id                = var.windows_template_vm_id
    vm_name              = "${var.windows_template_name}-builder"
    template_name        = var.windows_template_name
    template_description = "Windows GitHub Actions runner (${var.interactive_user != "" ? "interactive" : "SYSTEM/Session 0"}). Built {{ timestamp }}."
  }
  # Base: install + every Windows update + optimize.ps1, WinRM left on for the layer. Rebuild monthly.
  source "source.proxmox-iso.windows_runner" {
    name                 = "windows_base"
    vm_id                = var.windows_base_vm_id
    vm_name              = "${var.windows_base_template_name}-builder"
    template_name        = var.windows_base_template_name
    template_description = "Patched Windows Server base for runner layers (build account + WinRM still enabled; not for runners). Built {{ timestamp }}."
  }
  # Runner layer on top of the base (fast path).
  sources = ["source.proxmox-clone.windows_layer"]

  # Stage the repo's Windows scripts. The installer copies Watch/Test from beside itself.
  provisioner "powershell" {
    inline = ["New-Item -ItemType Directory -Force -Path C:\\Windows\\Temp\\scaleset | Out-Null"]
  }
  provisioner "file" {
    sources = [
      "../scripts/windows/Install-JitRunnerWatcher.ps1",
      "../scripts/windows/Watch-JitRunner.ps1",
      "../scripts/windows/Test-JitConfig.ps1",
      "../scripts/windows/Initialize-InteractiveRunner.ps1",
      "windows/install-runner.ps1",
      "windows/install-extra-dirs.ps1",
      "windows/install-docker.ps1",
      "windows/install-tools.ps1",
      "windows/optimize.ps1",
      "windows/finalize.ps1",
    ]
    destination = "C:\\Windows\\Temp\\scaleset\\"
  }

  provisioner "powershell" {
    except = ["proxmox-iso.windows_base"]
    inline = [
      "& C:\\Windows\\Temp\\scaleset\\install-runner.ps1 -RunnerVersion '${var.runner_version}' -RunnerSha256 '${var.runner_sha256}'",
    ]
  }

  provisioner "powershell" {
    except = ["proxmox-iso.windows_base"]
    inline = ["& C:\\Windows\\Temp\\scaleset\\install-tools.ps1"]
  }

  # Docker Engine (Windows containers). The Containers feature needs the reboot; the update
  # round below reboots again as needed, so this one is cheap insurance for the docker service.
  provisioner "powershell" {
    except = ["proxmox-iso.windows_base"]
    inline = ["& C:\\Windows\\Temp\\scaleset\\install-docker.ps1 -Version '${var.docker_version}'"]
  }
  provisioner "windows-restart" {
    except = ["proxmox-iso.windows_base"]
  }

  # Optional toolchains baked into the image (var.windows_extra_dirs).
  provisioner "powershell" {
    except = ["proxmox-iso.windows_base"]
    environment_vars = [
      "EXTRA_DIRS_JSON=${jsonencode(var.windows_extra_dirs)}",
      "EXTRA_DIRS_CHECKS_JSON=${jsonencode(var.windows_extra_dirs_checks)}",
      "EXTRA_DIRS_TOKEN=${var.windows_extra_dirs_token}",
    ]
    inline = ["if ($env:EXTRA_DIRS_JSON -ne '{}') { & C:\\Windows\\Temp\\scaleset\\install-extra-dirs.ps1 }"]
  }

  # Fully patch Windows first (the Linux build does apt upgrade). The plugin reboots and
  # repeats until no updates remain.
  provisioner "windows-update" {
    except          = ["proxmox-clone.windows_layer"]
    pause_before    = "30s"
    search_criteria = "IsInstalled=0"
    filters = [
      "exclude:$_.Title -like '*Preview*'",
      "include:$true",
    ]
  }

  # After the update round (it must run before Windows Update is disabled).
  provisioner "powershell" {
    except = ["proxmox-clone.windows_layer"]
    inline = ["& C:\\Windows\\Temp\\scaleset\\optimize.ps1"]
  }

  provisioner "powershell" {
    except           = ["proxmox-iso.windows_base"]
    environment_vars = ["INTERACTIVE_PASSWORD=${var.interactive_password}"]
    inline = [
      var.interactive_user == "" ? "& C:\\Windows\\Temp\\scaleset\\Install-JitRunnerWatcher.ps1" : join("; ", [
        "$ErrorActionPreference = 'Stop'",
        "if (-not $env:INTERACTIVE_PASSWORD) { throw 'interactive_password is required with interactive_user.' }",
        "$pf = 'C:\\Windows\\Temp\\scaleset\\pw'",
        "[IO.File]::WriteAllText($pf, $env:INTERACTIVE_PASSWORD)",
        "& C:\\Windows\\Temp\\scaleset\\Initialize-InteractiveRunner.ps1 -UserName '${var.interactive_user}' -PasswordFile $pf",
        "& C:\\Windows\\Temp\\scaleset\\Install-JitRunnerWatcher.ps1 -InteractiveUser '.\\${var.interactive_user}'",
      ]),
    ]
  }

  provisioner "powershell" {
    except = ["proxmox-iso.windows_base"]
    inline = [
      "& C:\\Windows\\Temp\\scaleset\\finalize.ps1 -BuildUserName '${var.windows_build_username}' -InteractiveUser '${var.interactive_user}'",
      "Remove-Item -Recurse -Force C:\\Windows\\Temp\\scaleset",
    ]
  }

  # Base only: drop the staged scripts; keep the build account and WinRM so the layer can connect.
  provisioner "powershell" {
    only   = ["proxmox-iso.windows_base"]
    inline = ["Remove-Item -Recurse -Force C:\\Windows\\Temp\\scaleset"]
  }
}
