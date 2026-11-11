# Variables for the Windows runner template build. Provide values via
# `variables.auto.pkrvars.hcl` (gitignored) or -var/-var-file; see the .example.

# ---------- Proxmox connection ----------

variable "proxmox_url" {
  type        = string
  description = "Proxmox VE API endpoint, e.g. https://pve.example.com:8006/api2/json"
}

variable "proxmox_username" {
  type        = string
  description = "API token ID, e.g. packer@pve!automation"
}

variable "proxmox_token" {
  type        = string
  description = "API token secret. Prefer PKR_VAR_proxmox_token."
  sensitive   = true
}

variable "proxmox_insecure_skip_tls_verify" {
  type    = bool
  default = false
}

variable "proxmox_node" {
  type        = string
  description = "Proxmox node to run the build on."
}

# ---------- Template VM ----------

variable "template_vm_id" {
  type        = number
  default     = 9100
  description = "VMID the resulting template gets. Must NOT overlap the orchestrator's vmid_range."
}

variable "template_name" {
  type    = string
  default = "windows-runner-template"
}

variable "vm_cores" {
  type    = number
  default = 4
}

variable "vm_memory_mb" {
  type    = number
  default = 8192
}

variable "vm_disk_size_gb" {
  type    = number
  default = 80
}

variable "enable_tpm" {
  type        = bool
  default     = true
  description = "Attach a virtual TPM 2.0 (stored in storage_pool). Required by Windows 11; harmless for Server."
}

# ---------- Storage / network ----------

variable "storage_pool" {
  type        = string
  description = "Proxmox storage pool for the OS disk and EFI disk (e.g. local-lvm)."
}

variable "iso_storage_pool" {
  type        = string
  description = "Storage pool that holds (or will hold) the Windows and VirtIO ISOs."
}

variable "network_bridge" {
  type    = string
  default = "vmbr0"
}

variable "network_vlan_tag" {
  type    = number
  default = 0
}

# ---------- ISOs ----------

variable "windows_iso_url" {
  type        = string
  default     = ""
  description = "HTTPS URL of a Windows ISO that the Proxmox host downloads. Set this OR windows_iso_file. Microsoft's Windows 11 ISO links are generated on the download page and expire after about 24 hours, so a fresh link works for an immediate build; for repeat builds upload the ISO and use windows_iso_file. Server evaluation ISOs have a stable link (see the .example file)."
}

variable "windows_iso_file" {
  type        = string
  default     = ""
  description = "Proxmox storage path of an already-uploaded Windows ISO, e.g. 'local:iso/Win11_x64.iso'. Set this OR windows_iso_url."
}

variable "windows_iso_checksum" {
  type        = string
  default     = "none"
  description = "Checksum ('sha256:...') of the Windows ISO. Microsoft republishes evaluation ISOs, so no value is pinned here; set one for reproducible builds, or leave 'none'."
}

variable "windows_image_name" {
  type        = string
  default     = "Windows 11 Pro"
  description = "Edition to install, exactly as named inside the ISO's install.wim, e.g. 'Windows 11 Pro', or for Server 'Windows Server 2022 SERVERSTANDARD' (Desktop Experience) / '... SERVERSTANDARDCORE' (Server Core). Interactive sessions need a Desktop Experience edition."
}

variable "windows_driver_folder" {
  type        = string
  default     = "w11"
  description = "Folder under the VirtIO ISO's drivers (viostor, vioscsi, ...) matching your Windows version: w11, w10, 2k25, 2k22, 2k19."
}

variable "virtio_iso_url" {
  type        = string
  default     = "https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/virtio-win.iso"
  description = "URL of the virtio-win ISO that the Proxmox host downloads. Set to \"\" and use virtio_iso_file to use one you already have."
}

variable "virtio_iso_file" {
  type        = string
  default     = ""
  description = "Proxmox storage path of an already-uploaded virtio-win ISO, e.g. 'local:iso/virtio-win.iso'. Overrides virtio_iso_url when set."
}

variable "virtio_iso_checksum" {
  type        = string
  default     = "none"
  description = "Checksum ('sha256:...') of the virtio-win ISO, or 'none'. The 'stable' ISO changes over time, so none is pinned."
}

variable "iso_download_pve" {
  type        = bool
  default     = true
  description = "Have the Proxmox host download ISOs from the URLs directly instead of routing multi-GB files through the machine running Packer."
}

variable "windows_product_key" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Optional product key. Leave empty for evaluation media or to activate later."
}

# ---------- Build-time account ----------
#
# Used only while Packer provisions over WinRM. finalize.ps1 disables the
# account, WinRM and its firewall rule before the template is finalized.

variable "build_username" {
  type    = string
  default = "packer"
}

variable "build_password" {
  type      = string
  default   = "Packer-Build-1!"
  sensitive = true
}

# ---------- Runner ----------

variable "runner_version" {
  type        = string
  default     = "2.334.0"
  description = "GitHub Actions runner release. See https://github.com/actions/runner/releases."
}

variable "runner_sha256" {
  type        = string
  default     = ""
  description = "SHA-256 of actions-runner-win-x64-<version>.zip (published in the release notes). Strongly recommended; empty skips verification."
}

variable "runner_directory" {
  type    = string
  default = "C:\\actions-runner"
}

# ---------- Interactive session (optional) ----------

variable "interactive_user" {
  type        = string
  default     = ""
  description = "If set, create this local Administrator, enable automatic logon for it, and run the runner in its interactive desktop session instead of as SYSTEM in Session 0. Requires a Desktop Experience image."
}

variable "interactive_password" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Password for interactive_user. Stored in plaintext in the Winlogon registry key of the template (required by Windows auto-logon): use a dedicated account."
}
