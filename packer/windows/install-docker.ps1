[CmdletBinding()]
param(
    [string] $Version = ''   # e.g. 28.0.4; empty = newest on download.docker.com
)
# Installs the Windows Containers feature and the Docker Engine (Windows containers only), registered as an
# auto-start service. Run it before a reboot: the Containers feature is not active until then.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
Install-WindowsFeature -Name Containers | Out-Null
$base = 'https://download.docker.com/win/static/stable/x86_64/'
if (-not $Version) {
    $names = (Invoke-WebRequest -UseBasicParsing -Uri $base).Links.href | ForEach-Object { if ($_ -match '(?:^|/)docker-(\d+\.\d+\.\d+)\.zip$') { [version]$Matches[1] } }
    if (-not $names) { throw "No docker-<version>.zip found at $base" }
    $Version = ($names | Sort-Object -Descending | Select-Object -First 1).ToString()
}
$zip = Join-Path $env:TEMP "docker-$Version.zip"
Invoke-WebRequest -UseBasicParsing -Uri "${base}docker-$Version.zip" -OutFile $zip
Expand-Archive -LiteralPath $zip -DestinationPath $env:ProgramFiles -Force   # creates %ProgramFiles%\docker
Remove-Item -LiteralPath $zip -Force
$bin = Join-Path $env:ProgramFiles 'docker'
if (-not (Test-Path -LiteralPath (Join-Path $bin 'dockerd.exe'))) { throw 'dockerd.exe missing after extraction.' }
[Environment]::SetEnvironmentVariable('Path', "$([Environment]::GetEnvironmentVariable('Path', 'Machine'));$bin", 'Machine')
& (Join-Path $bin 'dockerd.exe') --register-service
if ($LASTEXITCODE -ne 0) { throw 'dockerd --register-service failed.' }
Set-Service docker -StartupType Automatic
# dockerd needs the Host Network Service (HNS) up at boot; without the dependency it can start first and exit.
sc.exe config docker depend= hns | Out-Null
sc.exe failure docker reset= 86400 actions= restart/5000/restart/5000/restart/5000 | Out-Null
Write-Host "Installed Docker Engine $Version; reboot to activate the Containers feature."
