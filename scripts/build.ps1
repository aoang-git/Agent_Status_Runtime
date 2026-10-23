param([switch]$Test)

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
. (Join-Path $PSScriptRoot 'processes.ps1')
$instances = @()
$payload = Join-Path $projectRoot 'runtime/cmd/agent-status-setup/payload/agent-statusd.exe'
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
    $instances = @(Stop-BuildInstances @((Join-Path $projectRoot 'bin/agent-statusd.exe'), (Join-Path $projectRoot 'bin/agent-status-setup.exe')))
    & $goBinary build -trimpath '-ldflags=-H=windowsgui' -o (Join-Path $projectRoot 'bin/agent-statusd.exe') ./cmd/agent-statusd
    if ($LASTEXITCODE -ne 0) { throw 'Build failed.' }
    Copy-Item -LiteralPath (Join-Path $projectRoot 'bin/agent-statusd.exe') -Destination $payload -Force
    & $goBinary build -trimpath '-ldflags=-H=windowsgui' -o (Join-Path $projectRoot 'bin/agent-status-setup.exe') ./cmd/agent-status-setup
    if ($LASTEXITCODE -ne 0) { throw 'Installer build failed.' }
    $savedOS = $env:GOOS
    $savedArch = $env:GOARCH
    try {
        foreach ($target in @(@('linux','amd64'),@('darwin','arm64'))) {
            $env:GOOS=$target[0]; $env:GOARCH=$target[1]
            & $goBinary build -trimpath -o (Join-Path $projectRoot "bin/agent-statusd-$($target[0])-$($target[1])") ./cmd/agent-statusd
            if ($LASTEXITCODE -ne 0) { throw 'Cross build failed.' }
        }
    } finally { $env:GOOS=$savedOS; $env:GOARCH=$savedArch }
    foreach ($name in @('agent-statusd.exe','agent-status-setup.exe','agent-statusd-linux-amd64','agent-statusd-darwin-arm64')) {
        if ((Get-Item -LiteralPath (Join-Path $projectRoot "bin/$name")).Length -lt 1024) { throw "Invalid build: $name" }
    }
} finally {
    if (Test-Path -LiteralPath $payload) { Remove-Item -LiteralPath $payload -Force }
    Restore-BuildInstances $instances
    Pop-Location
    $env:GOCACHE = $previousCache
    $env:GOMODCACHE = $previousModules
}
