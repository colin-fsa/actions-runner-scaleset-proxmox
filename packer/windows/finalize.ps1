[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string] $BuildUserName,
    [string] $InteractiveUser = ''
)
# Last provisioning step. Removes build-time access so clones don't ship it. Runs over WinRM, so the
# build account and WinRM are disabled but only take effect once this session ends; Packer then
# issues the shutdown.
$ErrorActionPreference = 'Stop'
Remove-NetFirewallRule -DisplayName 'Packer WinRM (build only)' -ErrorAction SilentlyContinue
Set-Item WSMan:\localhost\Service\AllowUnencrypted -Value $false
Set-Item WSMan:\localhost\Service\Auth\Basic -Value $false
Set-Service WinRM -StartupType Disabled
$winlogon = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'
if (-not $InteractiveUser) {
    # SYSTEM mode: no automatic logon should remain from setup.
    Set-ItemProperty -Path $winlogon -Name AutoAdminLogon -Value '0' -Type String
    Remove-ItemProperty -Path $winlogon -Name DefaultPassword -ErrorAction SilentlyContinue
}
# The task scheduler and runner state must be clean: no registration, no JIT, no marker.
foreach ($leftover in 'C:\ProgramData\ActionsRunnerScaleSet\jitconfig', 'C:\ProgramData\ActionsRunnerScaleSet\started') {
    if (Test-Path -LiteralPath $leftover) { throw "Unexpected leftover handoff state: $leftover" }
}
Disable-LocalUser -Name $BuildUserName
Clear-RecycleBin -Force -ErrorAction SilentlyContinue
Remove-Item -Path "$env:TEMP\*" -Recurse -Force -ErrorAction SilentlyContinue
