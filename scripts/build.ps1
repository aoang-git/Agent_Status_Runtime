param([switch]$Test)

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$localGo = Join-Path $projectRoot '.tools/go/bin/go.exe'
$goCommand = Get-Command go -ErrorAction SilentlyContinue
if (Test-Path -LiteralPath $localGo) {
    $goBinary = $localGo
} elseif ($goCommand) {
    $goBinary = $goCommand.Source
} else {
    throw 'Go 1.26 or newer is required.'
}

$previousCache = $env:GOCACHE
$previousModules = $env:GOMODCACHE
$env:GOCACHE = Join-Path $projectRoot '.tools/gocache'
$env:GOMODCACHE = Join-Path $projectRoot '.tools/gomodcache'
Push-Location (Join-Path $projectRoot 'runtime')
try {
    if ($Test) {
        & $goBinary test ./...
        if ($LASTEXITCODE -ne 0) { throw 'Tests failed.' }
        & $goBinary vet ./...
        if ($LASTEXITCODE -ne 0) { throw 'go vet failed.' }
    }
    New-Item -ItemType Directory -Force (Join-Path $projectRoot 'bin') | Out-Null
    & $goBinary build -trimpath -o (Join-Path $projectRoot 'bin/agent-statusd.exe') ./cmd/agent-statusd
    if ($LASTEXITCODE -ne 0) { throw 'Build failed.' }
} finally {
    Pop-Location
    $env:GOCACHE = $previousCache
    $env:GOMODCACHE = $previousModules
}
