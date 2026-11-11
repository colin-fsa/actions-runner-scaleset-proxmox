# Variables for the Windows runner template build (proxmox-windows.pkr.hcl).
#
# Connection, storage, network and runner_version settings are shared with the
# Ubuntu build; see variables.pkr.hcl. Only Windows-specific values live here.

# ---------- Template VM ----------

variable "windows_template_vm_id" {
  type        = number
  default     = 9100
  description = "VMID the resulting template gets. Must NOT overlap the orchestrator's vmid_range."
}

variable "windows_template_name" {
  type    = string
  default = "windows-runner-template"
}

variable "windows_vm_cores" {
  type    = number
  default = 4
}

variable "windows_vm_memory_mb" {
  type    = number
  default = 8192
}

variable "windows_vm_disk_size_gb" {
  type    = number
  default = 80
}

# ---------- ISOs ----------

variable "windows_iso_url" {
  type        = string
  default     = "https://software-static.download.prss.microsoft.com/sg/download/888969d5-f34g-4e03-ac9d-1f9786c66749/SERVER_EVAL_x64FRE_en-us.iso"
  description = "HTTPS URL of the Windows Server ISO that the Proxmox host downloads. Defaults to Microsoft's freely downloadable Server 2022 evaluation ISO (180-day evaluation; no login). The URL must end in a filename with an .iso extension (Proxmox requires one), so the go.microsoft.com redirect link does not work; if Microsoft rotates this direct link, resolve the current one from https://www.microsoft.com/evalcenter or upload the ISO and use windows_iso_file. For a licensed ISO, upload it and set windows_iso_file instead, which takes precedence."
}

variable "windows_iso_file" {
  type        = string
  default     = ""
  description = "Proxmox storage path of an already-uploaded Windows Server ISO, e.g. 'local:iso/windows-server-2022.iso'. When set, windows_iso_url is ignored."
}

variable "windows_iso_checksum" {
  type        = string
  default     = "none"
  description = "Checksum ('sha256:...') of the Windows ISO. Microsoft republishes evaluation ISOs, so no value is pinned here; set one for reproducible builds, or leave 'none'."
}

variable "windows_image_name" {
  type        = string
  default     = "Windows Server 2022 SERVERSTANDARD"
  description = "Edition to install, exactly as named inside the ISO's install.wim. '... SERVERSTANDARD' is Desktop Experience (required for interactive sessions); '... SERVERSTANDARDCORE' is Server Core."
}

variable "windows_driver_folder" {
  type        = string
  default     = "2k22"
  description = "Folder under the VirtIO ISO's drivers (viostor, vioscsi, ...) matching your Windows version (2k22 for Server 2022, 2k25 for Server 2025)."
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

# ---------- Build-time account ----------
#
# Used only while Packer provisions over WinRM; finalize.ps1 disables the account, WinRM
# and its firewall rule before the template is finalized.

variable "windows_build_username" {
  type    = string
  default = "packer"
}

variable "windows_build_password" {
  type      = string
  default   = "Packer-Build-1!"
  sensitive = true
}

# ---------- Runner ----------

# ---------- Runner ----------

variable "runner_sha256" {
  type        = string
  default     = ""
  description = "SHA-256 of actions-runner-win-x64-<version>.zip (published in the release notes). Strongly recommended; empty skips verification."
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

# ---------- Extra directories ----------

variable "windows_extra_dirs" {
  type        = map(string)
  default     = {}
  description = "Archives baked into the image: guest destination path => archive URL (zip or tar), e.g. { \"C:\\\\DelphiCI\" = \"https://api.github.com/repos/org/repo/zipball/ref\" }. A single top-level folder in the archive is flattened into the destination."
}

variable "windows_extra_dirs_token" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Bearer token sent when downloading windows_extra_dirs (e.g. a GitHub token for private repositories). Only used during the build."
}
