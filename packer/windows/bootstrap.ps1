# Runs once at first logon from the Autounattend CD. Installs the VirtIO driver bundle and QEMU guest
# agent from the VirtIO ISO (NetKVM gives the guest a network; the agent lets Packer find its IP),
# then enables WinRM so Packer can connect. Build-time only: finalize.ps1 turns WinRM back off.
$ErrorActionPreference = 'Stop'
$virtio = [IO.DriveInfo]::GetDrives() | Where-Object { $_.IsReady -and (Test-Path -LiteralPath (Join-Path $_.Name 'virtio-win-gt-x64.msi')) } | Select-Object -First 1
if (-not $virtio) { throw 'VirtIO ISO (virtio-win-gt-x64.msi) not found on any drive.' }
$msi = Join-Path $virtio.Name 'virtio-win-gt-x64.msi'
$p = Start-Process msiexec.exe -ArgumentList @('/i', "`"$msi`"", '/qn', '/norestart', 'ADDLOCAL=ALL') -Wait -PassThru
if ($p.ExitCode -notin 0, 3010) { throw "VirtIO installer failed: $($p.ExitCode)" }
$ga = Join-Path $virtio.Name 'guest-agent\qemu-ga-x86_64.msi'
$p = Start-Process msiexec.exe -ArgumentList @('/i', "`"$ga`"", '/qn', '/norestart') -Wait -PassThru
if ($p.ExitCode -notin 0, 3010) { throw "QEMU guest agent installer failed: $($p.ExitCode)" }
Set-Service QEMU-GA -StartupType Automatic
Start-Service QEMU-GA
# Treat the build network as private so WinRM can listen on it.
Get-NetConnectionProfile | Set-NetConnectionProfile -NetworkCategory Private -ErrorAction SilentlyContinue
Enable-PSRemoting -Force -SkipNetworkProfileCheck
Set-Item WSMan:\localhost\Service\AllowUnencrypted -Value $true
Set-Item WSMan:\localhost\Service\Auth\Basic -Value $true
Set-Item WSMan:\localhost\MaxTimeoutms -Value 1800000
Set-Service WinRM -StartupType Automatic
New-NetFirewallRule -DisplayName 'Packer WinRM (build only)' -Direction Inbound -Protocol TCP -LocalPort 5985 -Action Allow -Profile Any | Out-Null
