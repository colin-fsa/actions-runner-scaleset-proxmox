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

variable "windows_iso_file" {
  type        = string
  description = "Proxmox storage path of an uploaded Windows Server ISO, e.g. 'local:iso/windows-server-2022.iso'. Microsoft ISOs are not downloadable anonymously, so upload it first."
}

variable "windows_iso_checksum" {
  type        = string
  default     = "none"
  description = "Checksum of the Windows ISO ('sha256:...'), or 'none' to skip (the ISO is already on Proxmox)."
}

variable "windows_image_name" {
  type        = string
  default     = "Windows Server 2022 SERVERSTANDARD"
  description = "Edition to install, exactly as named inside the ISO's install.wim. '... SERVERSTANDARD' is Desktop Experience and '... SERVERSTANDARDCORE' is Server Core. Interactive sessions need Desktop Experience."
}

variable "windows_driver_folder" {
  type        = string
  default     = "2k22"
  description = "Folder under the VirtIO ISO's drivers (viostor, vioscsi, ...) matching your Windows version: 2k19, 2k22, 2k25, w10, w11."
}

variable "virtio_iso_file" {
  type        = string
  description = "Proxmox storage path of the virtio-win ISO, e.g. 'local:iso/virtio-win.iso'. Download from https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/."
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
