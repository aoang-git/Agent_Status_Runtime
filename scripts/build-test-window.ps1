param([switch]$Test)

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
. (Join-Path $PSScriptRoot 'processes.ps1')
$instances=@()
$publish=Join-Path $projectRoot '.tools/test-window-publish'
$dotnet = (Get-Command dotnet -ErrorAction Stop).Source
$saved = @{}
$settings = @{
    DOTNET_CLI_HOME = Join-Path $projectRoot '.tools/dotnet'
    DOTNET_CLI_TELEMETRY_OPTOUT = '1'
    DOTNET_GENERATE_ASPNET_CERTIFICATE = 'false'
    MSBuildEnableWorkloadResolver = 'false'
    NUGET_PACKAGES = Join-Path $projectRoot '.tools/nuget'
}
foreach ($key in $settings.Keys) {
    $saved[$key] = [Environment]::GetEnvironmentVariable($key, 'Process')
    [Environment]::SetEnvironmentVariable($key, $settings[$key], 'Process')
}
try {
    if ($Test) {
        & $dotnet test (Join-Path $projectRoot 'manager/test-window/Tests/Tests.csproj') --nologo -m:1 -nr:false -p:BuildInParallel=false -p:UseSharedCompilation=false
        if ($LASTEXITCODE -ne 0) { throw 'Window client tests failed.' }
    }
    $publish = Join-Path $projectRoot '.tools/test-window-publish'
    & $dotnet publish (Join-Path $projectRoot 'manager/test-window/App/AgentStatus.TestWindow.csproj') `
        -c Release -r win-x64 --self-contained true -p:PublishSingleFile=true `
        -p:IncludeNativeLibrariesForSelfExtract=true -p:EnableCompressionInSingleFile=true `
        -p:DebugType=embedded -o $publish --nologo
    if ($LASTEXITCODE -ne 0) { throw 'Window publish failed.' }
    New-Item -ItemType Directory -Force (Join-Path $projectRoot 'bin') | Out-Null
    $instances=@(Stop-BuildInstances @((Join-Path $projectRoot 'bin/agent-status-test.exe')))
    Copy-Item -LiteralPath (Join-Path $publish 'agent-status-test.exe') -Destination (Join-Path $projectRoot 'bin/agent-status-test.exe') -Force
    if ((Get-FileHash -LiteralPath (Join-Path $publish 'agent-status-test.exe')).Hash -ne (Get-FileHash -LiteralPath (Join-Path $projectRoot 'bin/agent-status-test.exe')).Hash) { throw 'Published file hash mismatch.' }
    Write-Output 'Built bin/agent-status-test.exe (self-contained win-x64).'
} finally {
    Restore-BuildInstances $instances
    $resolved=[IO.Path]::GetFullPath($publish)
    if ($resolved -eq [IO.Path]::GetFullPath((Join-Path $projectRoot '.tools/test-window-publish')) -and (Test-Path -LiteralPath $resolved)) { Remove-Item -LiteralPath $resolved -Recurse -Force }
    foreach ($key in $settings.Keys) { [Environment]::SetEnvironmentVariable($key, $saved[$key], 'Process') }
}
