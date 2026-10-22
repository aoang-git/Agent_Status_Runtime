#requires -Version 7.0
<# Best-effort v2 state report. Uses .NET only; stdout stays empty. #>
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$DataDir,
    [Parameter(Mandatory)][string]$Agent,
    [Parameter(Mandatory)][string]$Session,
    [Parameter(Mandatory)][string]$Operation,
    [Parameter(Mandatory)][ValidateSet('working','tool_activity','waiting_confirmation','idle','completed','interrupted','failed','unknown','offline')][string]$State,
    [string]$Producer = 'powershell-example',
    [string]$Namespace,
    [string]$EventId = [guid]::NewGuid().ToString(),
    [ValidateSet('Cli','Http')][string]$Mode = 'Cli',
    [string]$Exe = 'agent-statusd'
)
$ErrorActionPreference = 'Stop'
try {
    $event = @{ schema = 2; producer_id = $Producer; event_id = $EventId; agent = $Agent;
        session_id = $Session; operation_id = $Operation; kind = 'state.reported';
        reported_basis = 'self_report'; data = @{ state = $State } }
    if ($Namespace) { $event.session_namespace = $Namespace }
    $payload = [Text.Encoding]::UTF8.GetBytes(($event | ConvertTo-Json -Depth 3 -Compress))
    if ($payload.Length -gt 16384) { throw 'body_too_large' }
    if ($Mode -eq 'Cli') {
        $start = [Diagnostics.ProcessStartInfo]::new($Exe)
        $start.UseShellExecute = $false
        $start.CreateNoWindow = $true
        $start.RedirectStandardInput = $true
        $start.RedirectStandardOutput = $true
        $start.RedirectStandardError = $true
        $start.StandardInputEncoding = [Text.UTF8Encoding]::new($false)
        foreach ($arg in @('report','--data-dir',$DataDir,'--best-effort')) { $start.ArgumentList.Add($arg) }
        $child = [Diagnostics.Process]::Start($start)
        try {
            $stdout = $child.StandardOutput.ReadToEndAsync()
            $stderr = $child.StandardError.ReadToEndAsync()
            $child.StandardInput.BaseStream.Write($payload, 0, $payload.Length)
            $child.StandardInput.Close()
            if (-not $child.WaitForExit(1000)) { $child.Kill(); throw 'report_timeout' }
            if ($child.ExitCode -ne 0 -or $stderr.GetAwaiter().GetResult().Length -gt 0) { throw 'report_unavailable' }
            $null = $stdout.GetAwaiter().GetResult()
        } finally { $child.Dispose() }
    } else {
        $handler = [Net.Http.SocketsHttpHandler]::new()
        $handler.UseProxy = $false
        $handler.AllowAutoRedirect = $false
        $http = [Net.Http.HttpClient]::new($handler)
        $http.Timeout = [Threading.Timeout]::InfiniteTimeSpan
        $stop = [Threading.CancellationTokenSource]::new(1000)
        try {
            for ($attempt = 0; $attempt -lt 2; $attempt++) {
                $configPath = Join-Path $DataDir 'private/ingest.json'
                $raw = [IO.File]::ReadAllBytes($configPath)
                if ($raw.Length -gt 16384) { throw 'invalid_connection' }
                $config = [Text.Encoding]::UTF8.GetString($raw) | ConvertFrom-Json
                $address = [uri]$config.url
                $loopback = $null
                if ($address.Scheme -ne 'http' -or $address.UserInfo -or $address.Query -or $address.Fragment -or
                    $address.AbsolutePath -ne '/' -or -not [Net.IPAddress]::TryParse($address.DnsSafeHost, [ref]$loopback) -or
                    -not [Net.IPAddress]::IsLoopback($loopback) -or $config.token -isnot [string] -or
                    -not $config.token -or $config.token.Length -gt 4096 -or $config.token -match '[\x00-\x1f\x7f-\x9f]') { throw 'invalid_connection' }
                $request = [Net.Http.HttpRequestMessage]::new([Net.Http.HttpMethod]::Post, [uri]::new($address, '/v2/ingest'))
                $request.Headers.Authorization = [Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer', $config.token)
                $request.Content = [Net.Http.ByteArrayContent]::new($payload)
                $request.Content.Headers.ContentType = [Net.Http.Headers.MediaTypeHeaderValue]::new('application/json')
                $response = $null
                try {
                    $response = $http.SendAsync($request, [Net.Http.HttpCompletionOption]::ResponseHeadersRead, $stop.Token).GetAwaiter().GetResult()
                    if ([int]$response.StatusCode -eq 401 -and $attempt -eq 0) { continue }
                    if ([int]$response.StatusCode -notin @(200,201)) { throw 'report_http_error' }
                    $stream = $response.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
                    $body = [IO.MemoryStream]::new()
                    try {
                        $buffer = [byte[]]::new(1024)
                        while (($read = $stream.ReadAsync($buffer, 0, $buffer.Length, $stop.Token).GetAwaiter().GetResult()) -gt 0) {
                            if ($body.Length + $read -gt 4096) { throw 'invalid_ack' }
                            $body.Write($buffer, 0, $read)
                        }
                        $ack = [Text.Encoding]::UTF8.GetString($body.ToArray()) | ConvertFrom-Json
                        if ($ack.schema -ne 2 -or $ack.id -lt 1 -or -not $ack.epoch) { throw 'invalid_ack' }
                    } finally { $body.Dispose(); $stream.Dispose() }
                    break
                } catch [Net.Http.HttpRequestException] {
                    if ($attempt -gt 0 -or $stop.IsCancellationRequested) { throw }
                } finally {
                    if ($null -ne $response) { $response.Dispose() }
                    $request.Dispose()
                }
            }
        } finally { $stop.Dispose(); $http.Dispose() }
    }
} catch {
    # Never print exception text, payload, credentials or ACK into a Hook stream.
    [Console]::Error.WriteLine('report_unavailable')
}
exit 0
