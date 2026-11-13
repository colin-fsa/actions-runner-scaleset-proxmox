# Installs Node.js LTS (with npm), Git for Windows and PowerShell 7 (pwsh), which the GitHub-hosted Windows images ship on PATH.
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

# PowerShell 7 ships a zip on recent releases (no MSI): extract it where the MSI would have put it.
$ps = Invoke-RestMethod 'https://api.github.com/repos/PowerShell/PowerShell/releases/latest'
$zipAsset = $ps.assets | Where-Object { $_.name -match '^PowerShell-[\d.]+-win-x64\.zip$' } | Select-Object -First 1
$pwshDir = Join-Path $env:ProgramFiles 'PowerShell\7'
$zip = Join-Path $env:TEMP $zipAsset.name
Invoke-WebRequest -UseBasicParsing -Uri $zipAsset.browser_download_url -OutFile $zip
Expand-Archive -LiteralPath $zip -DestinationPath $pwshDir -Force
Remove-Item $zip -Force
[Environment]::SetEnvironmentVariable('Path', "$([Environment]::GetEnvironmentVariable('Path', 'Machine'));$pwshDir", 'Machine')

$rel = Invoke-RestMethod 'https://api.github.com/repos/git-for-windows/git/releases/latest'
$asset = $rel.assets | Where-Object { $_.name -match '^Git-.*-64-bit\.exe$' } | Select-Object -First 1
$exe = Join-Path $env:TEMP $asset.name
Invoke-WebRequest -UseBasicParsing -Uri $asset.browser_download_url -OutFile $exe
$p = Start-Process $exe -ArgumentList '/VERYSILENT /NORESTART /SUPPRESSMSGBOXES' -Wait -PassThru
if ($p.ExitCode -ne 0) { throw "Git installer failed: $($p.ExitCode)" }
Remove-Item $exe -Force

$env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
Write-Host "node $(node --version), npm $(npm --version), $(git --version), pwsh $(pwsh -NoProfile -Command $PSVersionTable.PSVersion.ToString())"
