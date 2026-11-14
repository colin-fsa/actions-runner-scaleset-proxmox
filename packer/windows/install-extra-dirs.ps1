[CmdletBinding()]
param()
# Downloads each archive in $env:EXTRA_DIRS_JSON ({"C:\\Dest": "https://...zip"}) and extracts it to its
# destination, so large toolchains are baked into the template instead of copied per runner. A single
# top-level folder in the archive (as GitHub zipballs have) is flattened into the destination.
# $env:EXTRA_DIRS_TOKEN, if set, is sent as a bearer token (private GitHub repositories). GitHub release-asset API URLs
# (.../releases/assets/<id>) are fetched with Accept: application/octet-stream and the redirect followed without the token.
# $env:EXTRA_DIRS_CHECKS_JSON optionally maps a destination to {sha256, version, require_files}; any mismatch throws.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$dirs = $env:EXTRA_DIRS_JSON | ConvertFrom-Json
$checks = if ($env:EXTRA_DIRS_CHECKS_JSON) { $env:EXTRA_DIRS_CHECKS_JSON | ConvertFrom-Json } else { $null }
$headers = @{}
if ($env:EXTRA_DIRS_TOKEN) { $headers['Authorization'] = "Bearer $env:EXTRA_DIRS_TOKEN" }
foreach ($d in $dirs.PSObject.Properties) {
    $dest = $d.Name
    if (Test-Path -LiteralPath $dest) { throw "$dest already exists in the image." }
    $work = Join-Path $env:TEMP ('extra-' + [guid]::NewGuid())
    New-Item -ItemType Directory -Path $work | Out-Null
    $archive = Join-Path $work 'archive'
    if (Test-Path -LiteralPath $d.Value -PathType Leaf) { $archive = $d.Value }   # local file (e.g. from an attached ISO)
    elseif ($d.Value -match '^https://api\.github\.com/repos/[^/]+/[^/]+/releases/assets/\d+$') {
        $h = $headers + @{ Accept = 'application/octet-stream'; 'User-Agent' = 'packer-extra-dirs' }
        $url = $null
        try { Invoke-WebRequest -UseBasicParsing -Uri $d.Value -Headers $h -MaximumRedirection 0 -OutFile $archive | Out-Null }
        catch {
            $r = $_.Exception.Response
            if (-not $r) { throw "Asset request to $($d.Value) failed: $($_.Exception.Message)" }
            $url = $r.Headers['Location']
            if (-not $url) { throw "Asset request returned HTTP $([int]$r.StatusCode) without a redirect: $($_.Exception.Message)" }
        }
        if (-not $url) { throw "Asset request to $($d.Value) returned no redirect (file written directly?)." }
        Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $archive   # pre-signed storage URL: must not carry the token
    }
    else { Invoke-WebRequest -UseBasicParsing -Uri $d.Value -Headers $headers -OutFile $archive }
    $chk = if ($checks) { $checks.PSObject.Properties[$dest].Value } else { $null }
    if ($chk -and $chk.sha256) {
        $got = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash
        if ($got -ne $chk.sha256.ToUpper()) { throw "SHA-256 of $($d.Value) is $got, expected $($chk.sha256)." }
        Write-Host "SHA-256 verified for $dest"
    }
    $out = Join-Path $work 'out'
    New-Item -ItemType Directory -Path $out | Out-Null
    & tar.exe -xf $archive -C $out   # bsdtar: handles zip/tar.gz and archives over 2 GB
    if ($LASTEXITCODE -ne 0) { throw "Extracting $($d.Value) failed." }
    $items = @(Get-ChildItem -LiteralPath $out -Force | Where-Object Name -ne '__MACOSX')
    if ($items.Count -eq 1 -and -not $items[0].PSIsContainer -and $items[0].Extension -eq '.zip') {   # archive wrapping an inner zip
        $inner = Join-Path $work 'inner'
        New-Item -ItemType Directory -Path $inner | Out-Null
        & tar.exe -xf $items[0].FullName -C $inner
        if ($LASTEXITCODE -ne 0) { throw "Extracting inner archive $($items[0].Name) failed." }
        $out = $inner
        $items = @(Get-ChildItem -LiteralPath $out -Force | Where-Object Name -ne '__MACOSX')
    }
    $src = if ($items.Count -eq 1 -and $items[0].PSIsContainer) { $items[0].FullName } else { $out }
    $parent = Split-Path -Parent $dest
    if (-not (Test-Path -LiteralPath $parent)) { New-Item -ItemType Directory -Path $parent -Force | Out-Null }
    Move-Item -LiteralPath $src -Destination $dest
    if ($chk -and $chk.version) { Set-Content -LiteralPath (Join-Path $dest '.version') -Value $chk.version -NoNewline -Encoding ascii }
    foreach ($f in @($chk.require_files)) { if ($f -and -not (Test-Path -LiteralPath (Join-Path $dest $f))) { throw "$dest is missing required file $f." } }
    Remove-Item -LiteralPath $work -Recurse -Force
    Write-Host "Installed $dest"
}
