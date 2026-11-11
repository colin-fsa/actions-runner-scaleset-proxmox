[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string] $RunnerVersion,
    [string] $RunnerSha256 = '',
    [string] $RunnerDirectory = 'C:\actions-runner'
)
# Downloads the GitHub Actions runner into RunnerDirectory. It is not registered: the orchestrator
# supplies a single-use JIT config per clone.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$zip = Join-Path $env:TEMP "actions-runner-win-x64-$RunnerVersion.zip"
Invoke-WebRequest -UseBasicParsing -Uri "https://github.com/actions/runner/releases/download/v$RunnerVersion/actions-runner-win-x64-$RunnerVersion.zip" -OutFile $zip
if ($RunnerSha256) {
    $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $zip).Hash
    if ($actual -ne $RunnerSha256.ToUpperInvariant()) { throw "Runner checksum mismatch: expected $RunnerSha256, got $actual." }
} else {
    Write-Warning 'runner_sha256 not set: the runner download was not verified.'
}
New-Item -ItemType Directory -Path $RunnerDirectory -Force | Out-Null
Expand-Archive -LiteralPath $zip -DestinationPath $RunnerDirectory -Force
Remove-Item -LiteralPath $zip -Force
if (-not (Test-Path -LiteralPath (Join-Path $RunnerDirectory 'run.cmd'))) { throw 'run.cmd missing after extraction.' }
