param([double]$Hours=72,[string]$OutputDirectory='')
$ErrorActionPreference='Stop'
$workspace=Split-Path -Parent $PSScriptRoot
if (-not $OutputDirectory) { $OutputDirectory=Join-Path $workspace ('.runtime/qa/soak-'+[DateTime]::Now.ToString('yyyyMMdd-HHmmss')) }
$resolved=[IO.Path]::GetFullPath($OutputDirectory)
$qaRoot=[IO.Path]::GetFullPath((Join-Path $workspace '.runtime/qa'))+[IO.Path]::DirectorySeparatorChar
if (-not $resolved.StartsWith($qaRoot,[StringComparison]::OrdinalIgnoreCase)) { throw 'Soak output must remain within workspace .runtime/qa.' }
New-Item -ItemType Directory -Force $resolved | Out-Null
$logs=Join-Path $resolved 'synthetic-sessions'
$data=Join-Path $resolved 'runtime'
New-Item -ItemType Directory -Force $logs | Out-Null
$log=Join-Path $logs 'session.jsonl'
$header='{"type":"session_meta","payload":{"id":"soak-session","source":"cli"}}'+[Environment]::NewLine
[IO.File]::WriteAllText($log,$header)
$exe=Join-Path $workspace 'bin/agent-statusd.exe'
$arguments='serve --background --listen 127.0.0.1:0 --data-dir "'+$data+'" --root "codex='+$logs+'"'
$process=Start-Process -FilePath $exe -ArgumentList $arguments -PassThru -WindowStyle Hidden -WorkingDirectory $workspace
$started=[DateTime]::UtcNow
$finish=$started.AddHours($Hours)
$statusPath=Join-Path $resolved 'status.json'
$csv=Join-Path $resolved 'samples.csv'
$lastSample=$started.AddMinutes(-1)
$previousCPU=0
$previousAt=$started
$index=0
$broadcasts=0
$client=$null
$reader=$null
function Read-Frame {
    $lines=[Collections.Generic.List[string]]::new()
    while($true){
        $pending=$reader.ReadLineAsync()
        if(-not $pending.Wait(10000)){throw 'SSE did not deliver activity within 10 seconds.'}
        $line=$pending.GetAwaiter().GetResult()
        if($null -eq $line){throw 'SSE disconnected.'}
        if($line -eq ''){if($lines.Count -gt 0){return ($lines -join "\n")};continue}
        if(-not $line.StartsWith(':')){$lines.Add($line)}
    }
}
try {
    $deadline=[DateTime]::UtcNow.AddSeconds(20)
    $connectionPath=Join-Path $data 'private/connection.json'
    while(-not(Test-Path -LiteralPath $connectionPath)){
        if($process.HasExited){throw 'Fixture Runtime exited during startup.'}
        if([DateTime]::UtcNow -gt $deadline){throw 'Fixture Runtime startup timeout.'}
        Start-Sleep -Milliseconds 100
    }
    $c=Get-Content -Raw -LiteralPath $connectionPath|ConvertFrom-Json
    $handler=[Net.Http.SocketsHttpHandler]::new();$handler.UseProxy=$false;$handler.ConnectTimeout=[TimeSpan]::FromSeconds(5)
    $client=[Net.Http.HttpClient]::new($handler);$client.Timeout=[Threading.Timeout]::InfiniteTimeSpan
    $request=[Net.Http.HttpRequestMessage]::new([Net.Http.HttpMethod]::Get,$c.url+'/v3/events')
    $request.Headers.Authorization=[Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer',$c.token)
    $response=$client.SendAsync($request,[Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
    $null=$response.EnsureSuccessStatusCode()
    $reader=[IO.StreamReader]::new($response.Content.ReadAsStream())
    if((Read-Frame) -notmatch 'event: ready'){throw 'SSE ready missing.'}
    while([DateTime]::UtcNow -lt $finish){
        $current=[DateTime]::UtcNow
        $phase=$index%4
        $turn=[Math]::Floor($index/4)
        $payload=switch($phase){
            0 { @{type='task_started';turn_id="$turn"} }
            1 { @{type='custom_tool_call';call_id="tool-$turn"} }
            2 { @{type='custom_tool_call_output';call_id="tool-$turn"} }
            3 { @{type='task_complete';turn_id="$turn"} }
        }
        $record=@{type=$(if($phase -in @(1,2)){'response_item'}else{'event_msg'});timestamp=$current.ToString('o');payload=$payload}
        [IO.File]::AppendAllText($log,($record|ConvertTo-Json -Compress)+[Environment]::NewLine)
        $index++
        $frame=Read-Frame
        if($frame -notmatch 'event: fact'){throw 'Unexpected SSE frame during fixed workload.'}
        $broadcasts++
        if($current-$lastSample -ge [TimeSpan]::FromMinutes(1)){
            $c=Get-Content -Raw -LiteralPath $connectionPath|ConvertFrom-Json
            $health=Invoke-RestMethod -Uri ($c.url+'/v3/health') -Headers @{Authorization='Bearer '+$c.token} -TimeoutSec 5
            $p=Get-Process -Id $process.Id -ErrorAction Stop
            $seconds=($current-$previousAt).TotalSeconds
            $row=[pscustomobject]@{utc=$current.ToString('o');elapsed_hours=($current-$started).TotalHours;cpu_percent=$(if($seconds -gt 0){100*($p.CPU-$previousCPU)/$seconds/[Environment]::ProcessorCount}else{0});working_set=$p.WorkingSet64;private_bytes=$p.PrivateMemorySize64;handles=$p.HandleCount;broadcasts=$broadcasts;events=$health.queue.events;bytes=$health.queue.bytes;dropped=$health.queue.dropped;last_scan=$health.last_scan_at;watcher=$health.watcher}
            $row|Export-Csv -LiteralPath $csv -Append -NoTypeInformation
            if($health.queue.events -gt 4096 -or $health.queue.bytes -gt 8388608){throw 'Runtime queue exceeded limit.'}
            if($current-[DateTime]$health.last_scan_at -gt [TimeSpan]::FromMinutes(2)){throw 'Runtime scan stopped progressing.'}
            @{state='running';started_at=$started.ToString('o');target_hours=$Hours;elapsed_hours=($current-$started).TotalHours;pid=$process.Id;last_sample=$row}|ConvertTo-Json -Depth 4|Set-Content -LiteralPath $statusPath
            $previousCPU=$p.CPU;$previousAt=$current;$lastSample=$current
        }
        if((Get-Item -LiteralPath $log).Length -gt 1MB){[IO.File]::WriteAllText($log,$header)}
        Start-Sleep -Seconds 5
    }
    @{state='completed';started_at=$started.ToString('o');target_hours=$Hours;elapsed_hours=([DateTime]::UtcNow-$started).TotalHours}|ConvertTo-Json|Set-Content -LiteralPath $statusPath
} catch {
    @{state='failed';started_at=$started.ToString('o');target_hours=$Hours;elapsed_hours=([DateTime]::UtcNow-$started).TotalHours;error=$_.Exception.Message}|ConvertTo-Json|Set-Content -LiteralPath $statusPath
    throw
} finally {
    if($reader){$reader.Dispose()};if($client){$client.Dispose()}
    foreach($candidate in @(Get-CimInstance Win32_Process -Filter "Name='agent-statusd.exe'" -ErrorAction SilentlyContinue)){
        if($candidate.ExecutablePath -eq $exe -and $candidate.CommandLine.Contains($data)){Stop-Process -Id $candidate.ProcessId -Force}
    }
}
