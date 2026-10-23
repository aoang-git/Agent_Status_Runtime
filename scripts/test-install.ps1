param([string]$Workspace = (Split-Path -Parent $PSScriptRoot))
$ErrorActionPreference='Stop'
$testRoot=Join-Path $Workspace ('.runtime/qa/install-smoke-'+[Guid]::NewGuid().ToString('N'))
$local=Join-Path $testRoot 'Local'
$roaming=Join-Path $testRoot 'Roaming'
New-Item -ItemType Directory -Force $local,$roaming | Out-Null
$previousLocal=$env:LOCALAPPDATA
$previousRoaming=$env:APPDATA
$env:LOCALAPPDATA=$local
$env:APPDATA=$roaming
$exe=Join-Path $local 'Programs/AgentStatusRuntime/agent-statusd.exe'
$data=Join-Path $roaming 'AgentStatusRuntime'
$results=@()
function Read-Connection { Get-Content -Raw -LiteralPath (Join-Path $data 'private/connection.json') | ConvertFrom-Json }
function Wait-Service([int]$OldPid=0,[int]$Timeout=80) {
    $deadline=[DateTime]::UtcNow.AddSeconds($Timeout)
    while([DateTime]::UtcNow -lt $deadline) {
        try {
            $c=Read-Connection
            if ($c.pid -eq $OldPid) { Start-Sleep -Milliseconds 250; continue }
            $info=Invoke-RestMethod -Uri ($c.url+'/v3/info') -Headers @{Authorization='Bearer '+$c.token} -TimeoutSec 2
            if($info.schema -eq 3 -and $info.epoch -eq $c.epoch){ return $c }
        } catch {}
        Start-Sleep -Milliseconds 250
    }
    throw 'Service did not become healthy.'
}
try {
    $setup=Join-Path $Workspace 'bin/agent-status-setup.exe'
    $installer=Start-Process -FilePath $setup -ArgumentList '--silent' -Wait -PassThru -WindowStyle Hidden -RedirectStandardError (Join-Path $testRoot 'installer-error.txt')
    if($installer.ExitCode -ne 0){ throw ("Installer failed: "+(Get-Content -Raw -LiteralPath (Join-Path $testRoot 'installer-error.txt'))) }
    $first=Wait-Service
    $results+='install: pass'
    $installer=Start-Process -FilePath $setup -ArgumentList '--silent' -Wait -PassThru -WindowStyle Hidden
    if($installer.ExitCode -ne 0){ throw "Reinstall failed: $($installer.ExitCode)" }
    $second=Wait-Service $first.pid
    $results+='reinstall: pass'
    $stop=Start-Process -FilePath $exe -ArgumentList 'stop' -Wait -PassThru -WindowStyle Hidden
    if($stop.ExitCode -ne 0){throw 'Stop failed.'}
    if(Get-Process -Id $second.pid -ErrorAction SilentlyContinue){throw 'Stop did not stop service.'}
    $results+='stop: pass'
    $start=Start-Process -FilePath $exe -ArgumentList 'start' -Wait -PassThru -WindowStyle Hidden
    if($start.ExitCode -ne 0){throw 'Start failed.'}
    $third=Wait-Service
    $results+='start: pass'
    $running=Get-CimInstance Win32_Process -Filter "ProcessId=$($third.pid)"
    if($running.ExecutablePath -ne $exe -or $running.CommandLine -notlike '*--data-dir*'){throw 'Unexpected running process.'}
    Stop-Process -Id $third.pid -Force
    $timer=[Diagnostics.Stopwatch]::StartNew()
    $fourth=Wait-Service $third.pid
    $results+="forced-exit recovery: pass ($([Math]::Round($timer.Elapsed.TotalSeconds,1))s)"
    $uninstall=Start-Process -FilePath $exe -ArgumentList 'uninstall' -Wait -PassThru -WindowStyle Hidden
    if($uninstall.ExitCode -ne 0){throw 'Uninstall failed.'}
    $deadline=[DateTime]::UtcNow.AddSeconds(10)
    while((Test-Path -LiteralPath $exe) -and [DateTime]::UtcNow -lt $deadline){Start-Sleep -Milliseconds 250}
    if(Test-Path -LiteralPath $exe){throw 'Uninstall left executable.'}
    $results+='uninstall: pass'
    $results | Set-Content -LiteralPath (Join-Path $testRoot 'result.txt')
    $results
    Write-Output $testRoot
} finally {
    # Cleanup only this test installation; never touch an existing user installation.
    if(Test-Path -LiteralPath $exe){Start-Process -FilePath $exe -ArgumentList 'uninstall' -Wait -WindowStyle Hidden}
    $env:LOCALAPPDATA=$previousLocal
    $env:APPDATA=$previousRoaming
}
