# Installs Node.js LTS (with npm) and Git for Windows, which the GitHub-hosted Windows images ship on PATH.
# pnpm/action-setup, actions/checkout fallbacks and many workflows call node/npm/git directly.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

function Install-Msi($url, $name) {
    $f = Join-Path $env:TEMP $name
    Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $f
    $p = Start-Process msiexec.exe -ArgumentList "/i `"$f`" /qn /norestart" -Wait -PassThru
    if ($p.ExitCode -notin 0, 3010) { throw "$name failed: $($p.ExitCode)" }
    Remove-Item $f -Force
}

$lts = (Invoke-RestMethod 'https://nodejs.org/dist/index.json') | Where-Object { $_.lts } | Select-Object -First 1
Install-Msi "https://nodejs.org/dist/$($lts.version)/node-$($lts.version)-x64.msi" 'node.msi'

$rel = Invoke-RestMethod 'https://api.github.com/repos/git-for-windows/git/releases/latest'
$asset = $rel.assets | Where-Object { $_.name -match '^Git-.*-64-bit\.exe$' } | Select-Object -First 1
$exe = Join-Path $env:TEMP $asset.name
Invoke-WebRequest -UseBasicParsing -Uri $asset.browser_download_url -OutFile $exe
$p = Start-Process $exe -ArgumentList '/VERYSILENT /NORESTART /SUPPRESSMSGBOXES' -Wait -PassThru
if ($p.ExitCode -ne 0) { throw "Git installer failed: $($p.ExitCode)" }
Remove-Item $exe -Force

$env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
Write-Host "node $(node --version), npm $(npm --version), $(git --version)"
