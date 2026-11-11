[CmdletBinding()]
param()
# Downloads each archive in $env:EXTRA_DIRS_JSON ({"C:\\Dest": "https://...zip"}) and extracts it to its
# destination, so large toolchains are baked into the template instead of copied per runner. A single
# top-level folder in the archive (as GitHub zipballs have) is flattened into the destination.
# $env:EXTRA_DIRS_TOKEN, if set, is sent as a bearer token (private GitHub repositories).
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$dirs = $env:EXTRA_DIRS_JSON | ConvertFrom-Json
$headers = @{}
if ($env:EXTRA_DIRS_TOKEN) { $headers['Authorization'] = "Bearer $env:EXTRA_DIRS_TOKEN" }
foreach ($d in $dirs.PSObject.Properties) {
    $dest = $d.Name
    if (Test-Path -LiteralPath $dest) { throw "$dest already exists in the image." }
    $work = Join-Path $env:TEMP ('extra-' + [guid]::NewGuid())
    New-Item -ItemType Directory -Path $work | Out-Null
    $archive = Join-Path $work 'archive'
    if (Test-Path -LiteralPath $d.Value -PathType Leaf) { $archive = $d.Value }   # local file (e.g. from an attached ISO)
    else { Invoke-WebRequest -UseBasicParsing -Uri $d.Value -Headers $headers -OutFile $archive }
    $out = Join-Path $work 'out'
    New-Item -ItemType Directory -Path $out | Out-Null
    & tar.exe -xf $archive -C $out   # bsdtar: handles zip/tar.gz and archives over 2 GB
    if ($LASTEXITCODE -ne 0) { throw "Extracting $($d.Value) failed." }
    $items = @(Get-ChildItem -LiteralPath $out -Force)
    $src = if ($items.Count -eq 1 -and $items[0].PSIsContainer) { $items[0].FullName } else { $out }
    $parent = Split-Path -Parent $dest
    if (-not (Test-Path -LiteralPath $parent)) { New-Item -ItemType Directory -Path $parent -Force | Out-Null }
    Move-Item -LiteralPath $src -Destination $dest
    Remove-Item -LiteralPath $work -Recurse -Force
    Write-Host "Installed $dest"
}
