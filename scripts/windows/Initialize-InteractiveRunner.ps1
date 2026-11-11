[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string] $UserName,
    # File holding the account password (no trailing newline). It is deleted after reading.
    [Parameter(Mandatory)] [string] $PasswordFile
)
# Creates (or updates) a local Administrator account and configures automatic logon for it, so
# Install-JitRunnerWatcher.ps1 -InteractiveUser can start the runner in its desktop session.
# Never prints the password. Run elevated (the controller runs it as SYSTEM via the guest agent).
$ErrorActionPreference = 'Stop'
if ($UserName -notmatch '^[A-Za-z0-9_.-]{1,20}$') { throw 'UserName must be 1-20 characters of letters, digits, _, . or -.' }
try {
    $plain = [System.IO.File]::ReadAllText($PasswordFile)
} finally {
    Remove-Item -LiteralPath $PasswordFile -Force -ErrorAction SilentlyContinue
}
if ([string]::IsNullOrEmpty($plain)) { throw 'Password file was empty.' }
$secure = ConvertTo-SecureString -String $plain -AsPlainText -Force
if (Get-LocalUser -Name $UserName -ErrorAction SilentlyContinue) {
    Set-LocalUser -Name $UserName -Password $secure -PasswordNeverExpires $true
} else {
    New-LocalUser -Name $UserName -Password $secure -PasswordNeverExpires -AccountNeverExpires -UserMayNotChangePassword | Out-Null
}
Enable-LocalUser -Name $UserName
$admins = Get-LocalGroupMember -SID 'S-1-5-32-544' -ErrorAction SilentlyContinue | Where-Object { $_.Name -like "*\$UserName" }
if (-not $admins) { Add-LocalGroupMember -SID 'S-1-5-32-544' -Member $UserName }
# Automatic logon. DefaultPassword is stored in plaintext in the registry; use a dedicated account.
$winlogon = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'
Set-ItemProperty -Path $winlogon -Name AutoAdminLogon -Value '1' -Type String
Set-ItemProperty -Path $winlogon -Name DefaultUserName -Value $UserName -Type String
Set-ItemProperty -Path $winlogon -Name DefaultDomainName -Value '.' -Type String
Set-ItemProperty -Path $winlogon -Name DefaultPassword -Value $plain -Type String
Remove-ItemProperty -Path $winlogon -Name AutoLogonCount -ErrorAction SilentlyContinue
# Keep the session usable: no lock screen, no monitor/standby timeout.
$personalization = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\Personalization'
New-Item -Path $personalization -Force | Out-Null
Set-ItemProperty -Path $personalization -Name NoLockScreen -Value 1 -Type DWord
& "$env:SystemRoot\System32\powercfg.exe" /change monitor-timeout-ac 0 | Out-Null
& "$env:SystemRoot\System32\powercfg.exe" /change standby-timeout-ac 0 | Out-Null
Write-Host "Configured automatic logon for $UserName."
