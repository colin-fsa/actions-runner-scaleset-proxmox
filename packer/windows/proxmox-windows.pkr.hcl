# Windows 11 Pro runner template for the orchestrator.
#
# Produces a Proxmox template containing:
#   - Windows installed unattended on UEFI with VirtIO storage/network.
#   - The QEMU guest agent (required for JIT injection) and VirtIO drivers.
#   - The GitHub Actions runner at var.runner_directory (unregistered).
#   - The ActionsRunnerScaleSetWatcher scheduled task (scripts/windows/), either as SYSTEM in
#     Session 0 (default) or, when var.interactive_user is set, as that Administrator in an
#     interactive desktop session with automatic logon.
#
# Packer drives the guest over WinRM during the build only; finalize.ps1 removes that access.
#
# Run from this directory:
#   packer init .
#   packer validate .
#   packer build .

packer {
  required_plugins {
    proxmox = {
      version = ">= 1.2.0"
      source  = "github.com/hashicorp/proxmox"
    }
    # Community plugin: installs Windows Updates over WinRM and survives the reboots they need.
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

  vm_id                = var.template_vm_id
  vm_name              = "${var.template_name}-builder"
  template_name        = var.template_name
  template_description = "Windows GitHub Actions runner (${var.interactive_user != "" ? "interactive" : "SYSTEM/Session 0"}). Built {{ timestamp }}."

  os       = "win11" # Proxmox OS type for Windows 11 guests
  cpu_type = "host"
  cores    = var.vm_cores
  sockets  = 1
  memory   = var.vm_memory_mb
  machine  = "q35"
  bios     = "ovmf"
  efi_config {
    efi_storage_pool  = var.storage_pool
    efi_type          = "4m"
    pre_enrolled_keys = true # Windows Setup expects Microsoft's keys enrolled
  }

  # Windows 11 requires a TPM 2.0.
  dynamic "tpm_config" {
    for_each = var.enable_tpm ? [1] : []
    content {
      tpm_storage_pool = var.storage_pool
      tpm_version      = "v2.0"
    }
  }

  scsi_controller = "virtio-scsi-single"
  boot            = "order=scsi0;ide2;net0"

  disks {
    type         = "scsi"
    storage_pool = var.storage_pool
    disk_size    = "${var.vm_disk_size_gb}G"
    format       = "raw"
    io_thread    = true
    discard      = true
    ssd          = true
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
    iso_url          = var.windows_iso_url
    iso_file         = var.windows_iso_file
    iso_checksum     = var.windows_iso_checksum
    iso_storage_pool = var.iso_storage_pool
    iso_download_pve = var.windows_iso_url != "" && var.windows_iso_file == "" ? var.iso_download_pve : false
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
      "Autounattend.xml" = templatefile("${path.root}/answer_files/Autounattend.xml.pkrtpl.hcl", {
        image_name     = var.windows_image_name
        driver_folder  = var.windows_driver_folder
        product_key    = var.windows_product_key
        build_username = var.build_username
        build_password = var.build_password
      })
      "bootstrap.ps1" = file("${path.root}/scripts/bootstrap.ps1")
    }
    unmount = true
  }

  # UEFI media prompts "Press any key to boot from CD"; tap a key at the right moment.
  boot_wait    = "3s"
  boot_command = ["<spacebar><wait2><spacebar><wait2><spacebar><wait2><spacebar>"]

  # WinRM (build-time only). The address comes from the QEMU guest agent.
  communicator   = "winrm"
  winrm_username = var.build_username
  winrm_password = var.build_password
  winrm_use_ssl  = false
  winrm_insecure = true
  winrm_timeout  = "2h"

  tags = "windows;gh-runner;packer"
}

build {
  name    = "windows-runner"
  sources = ["source.proxmox-iso.windows_runner"]

  # Stage the repo's Windows scripts. The installer copies Watch/Test from beside itself.
  provisioner "powershell" {
    inline = ["New-Item -ItemType Directory -Force -Path C:\\Windows\\Temp\\scaleset | Out-Null"]
  }
  provisioner "file" {
    sources = [
      "../../scripts/windows/Install-JitRunnerWatcher.ps1",
      "../../scripts/windows/Watch-JitRunner.ps1",
      "../../scripts/windows/Test-JitConfig.ps1",
      "../../scripts/windows/Initialize-InteractiveRunner.ps1",
      "scripts/install-runner.ps1",
      "scripts/finalize.ps1",
    ]
    destination = "C:\\Windows\\Temp\\scaleset\\"
  }

  provisioner "powershell" {
    inline = [
      "& C:\\Windows\\Temp\\scaleset\\install-runner.ps1 -RunnerVersion '${var.runner_version}' -RunnerSha256 '${var.runner_sha256}' -RunnerDirectory '${var.runner_directory}'",
    ]
  }

  # Fully patch Windows first (the Linux build does apt upgrade). Updates need several reboot
  # rounds, so run update -> restart three times; later rounds are no-ops once current.
  provisioner "windows-update" {
    pause_before    = "30s"
    search_criteria = "IsInstalled=0"
    filters = [
      "exclude:$_.Title -like '*Preview*'",
      "include:$true",
    ]
    update_limit = 50
  }
  provisioner "windows-restart" {
    restart_timeout = "30m"
  }

  provisioner "windows-update" {
    pause_before    = "30s"
    search_criteria = "IsInstalled=0"
    filters = [
      "exclude:$_.Title -like '*Preview*'",
      "include:$true",
    ]
    update_limit = 50
  }
  provisioner "windows-restart" {
    restart_timeout = "30m"
  }

  provisioner "windows-update" {
    pause_before    = "30s"
    search_criteria = "IsInstalled=0"
    filters = [
      "exclude:$_.Title -like '*Preview*'",
      "include:$true",
    ]
    update_limit = 50
  }
  provisioner "windows-restart" {
    restart_timeout = "30m"
  }

  provisioner "powershell" {
    environment_vars = ["INTERACTIVE_PASSWORD=${var.interactive_password}"]
    inline = [
      var.interactive_user == "" ? "& C:\\Windows\\Temp\\scaleset\\Install-JitRunnerWatcher.ps1 -RunnerDirectory '${var.runner_directory}'" : join("; ", [
        "$ErrorActionPreference = 'Stop'",
        "if (-not $env:INTERACTIVE_PASSWORD) { throw 'interactive_password is required with interactive_user.' }",
        "$pf = 'C:\\Windows\\Temp\\scaleset\\pw'",
        "[IO.File]::WriteAllText($pf, $env:INTERACTIVE_PASSWORD)",
        "& C:\\Windows\\Temp\\scaleset\\Initialize-InteractiveRunner.ps1 -UserName '${var.interactive_user}' -PasswordFile $pf",
        "& C:\\Windows\\Temp\\scaleset\\Install-JitRunnerWatcher.ps1 -RunnerDirectory '${var.runner_directory}' -InteractiveUser '.\\${var.interactive_user}'",
      ]),
    ]
  }

  provisioner "powershell" {
    inline = [
      "& C:\\Windows\\Temp\\scaleset\\finalize.ps1 -BuildUserName '${var.build_username}' -InteractiveUser '${var.interactive_user}'",
      "Remove-Item -Recurse -Force C:\\Windows\\Temp\\scaleset",
    ]
  }
}
