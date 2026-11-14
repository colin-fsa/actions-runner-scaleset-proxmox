# Strips background work from the image: these runners are ephemeral (boot, run one job, vanish), so
# Defender, Windows Update, indexing, telemetry and maintenance only cost CPU, I/O and RAM.
# Best effort: each step is independent and a failure (e.g. Tamper Protection) is reported, not fatal.
$ErrorActionPreference = 'Continue'
function Step($name, [scriptblock]$b) { try { & $b; Write-Host "ok   $name" } catch { Write-Host "SKIP $name : $($_.Exception.Message)" } }
function Reg($path, $name, $value, $type = 'DWord') {
    New-Item -Path $path -Force | Out-Null
    New-ItemProperty -Path $path -Name $name -Value $value -PropertyType $type -Force | Out-Null
}

# Defender: runtime switches, then policy, then the services/drivers themselves.
Step 'defender runtime' {
    Set-MpPreference -DisableRealtimeMonitoring $true -DisableBehaviorMonitoring $true -DisableIOAVProtection $true `
        -DisableScriptScanning $true -DisableBlockAtFirstSeen $true -MAPSReporting 0 -SubmitSamplesConsent 2 -EnableNetworkProtection Disabled
}
Step 'defender policy' {
    $p = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows Defender'
    Reg $p DisableAntiSpyware 1; Reg $p DisableAntiVirus 1
    foreach ($n in 'DisableRealtimeMonitoring', 'DisableBehaviorMonitoring', 'DisableIOAVProtection', 'DisableOnAccessProtection', 'DisableScanOnRealtimeEnable') { Reg "$p\Real-Time Protection" $n 1 }
    Reg "$p\Spynet" SpynetReporting 0; Reg "$p\Spynet" SubmitSamplesConsent 2
}
Step 'smartscreen' { Reg 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\System' EnableSmartScreen 0; Reg 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer' SmartScreenEnabled 'Off' String }

# Services: Start=4 (disabled) in the registry also works for protected ones (WaaSMedicSvc) that Set-Service refuses.
$svcs = 'WinDefend', 'WdNisSvc', 'WdFilter', 'WdNisDrv', 'Sense', 'SgrmBroker', 'wuauserv', 'UsoSvc', 'WaaSMedicSvc', 'DoSvc',
        'WSearch', 'SysMain', 'DiagTrack', 'dmwappushservice', 'WerSvc', 'DPS', 'WdiServiceHost', 'WdiSystemHost', 'MapsBroker',
        'RetailDemo', 'wisvc', 'PcaSvc', 'TrkWks', 'defragsvc', 'edgeupdate', 'edgeupdatem', 'MicrosoftEdgeElevationService'
foreach ($s in $svcs) {
    Step "service $s" {
        $k = "HKLM:\SYSTEM\CurrentControlSet\Services\$s"
        if (-not (Test-Path $k)) { return }
        Stop-Service $s -Force -ErrorAction SilentlyContinue
        Set-ItemProperty -Path $k -Name Start -Value 4
    }
}

Step 'windows update policy' { $p = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\WindowsUpdate\AU'; Reg $p NoAutoUpdate 1; Reg $p AUOptions 1 }
Step 'telemetry/reporting' {
    Reg 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\DataCollection' AllowTelemetry 0
    Reg 'HKLM:\SOFTWARE\Microsoft\Windows\Windows Error Reporting' Disabled 1
    Reg 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\CloudContent' DisableWindowsConsumerFeatures 1
}
Step 'automatic maintenance' { Reg 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Schedule\Maintenance' MaintenanceDisabled 1 }

# Scheduled tasks that wake up on their own (defrag/trim, update orchestration, telemetry, Defender, diagnostics).
Step 'scheduled tasks' {
    Get-ScheduledTask | Where-Object {
        $_.TaskPath -match '\\Microsoft\\Windows\\(Defrag|UpdateOrchestrator|WindowsUpdate|Windows Defender|Application Experience|Customer Experience Improvement Program|DiskDiagnostic|Diagnosis|Maintenance|WDI|Windows Error Reporting|Chkdsk|SystemRestore|Power Efficiency Diagnostics|Feedback|Flighting|Management\\Provisioning|MemoryDiagnostic|Registry|Time Synchronization)\\'
    } | Disable-ScheduledTask -ErrorAction SilentlyContinue | Out-Null
}

# Filesystem/boot/power: no last-access or 8.3 writes, no hibernation file, no restore points, instant boot menu, full power.
Step 'ntfs' { fsutil behavior set disablelastaccess 1 | Out-Null; fsutil behavior set disable8dot3 1 | Out-Null }
Step 'hibernate' { powercfg /h off }
Step 'power plan' { powercfg /setactive SCHEME_MIN; powercfg /change monitor-timeout-ac 0; powercfg /change standby-timeout-ac 0 }
Step 'system restore' { Disable-ComputerRestore -Drive 'C:\' -ErrorAction SilentlyContinue; Reg 'HKLM:\SOFTWARE\Policies\Microsoft\Windows NT\SystemRestore' DisableSR 1 }
Step 'boot' { bcdedit /timeout 0 | Out-Null; bcdedit /set bootux disabled | Out-Null; bcdedit /set '{current}' bootstatuspolicy ignoreallfailures | Out-Null }
Step 'visual effects' { Reg 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\VisualEffects' VisualFXSetting 2 }
