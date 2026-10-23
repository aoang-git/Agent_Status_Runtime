$ErrorActionPreference = 'Stop'
function Stop-BuildInstances([string[]]$Paths) {
    $instances = @()
    foreach ($process in @(Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -and $Paths -contains $_.ExecutablePath })) {
        $line = $process.CommandLine
        $prefix = '"' + $process.ExecutablePath + '"'
        if ($line.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) { $arguments = $line.Substring($prefix.Length).Trim() }
        elseif ($line.StartsWith($process.ExecutablePath, [StringComparison]::OrdinalIgnoreCase)) { $arguments = $line.Substring($process.ExecutablePath.Length).Trim() }
        else { throw "Cannot verify command line for PID $($process.ProcessId)." }
        $instances += [pscustomobject]@{ Path=$process.ExecutablePath; Arguments=$arguments; WorkingDirectory=$projectRoot }
        Stop-Process -Id $process.ProcessId -Force -ErrorAction Stop
        Wait-Process -Id $process.ProcessId -Timeout 10 -ErrorAction SilentlyContinue
    }
    return $instances
}
function Restore-BuildInstances($Instances) {
    foreach ($instance in @($Instances)) {
        if ($instance.Arguments) { Start-Process -FilePath $instance.Path -ArgumentList $instance.Arguments -WindowStyle Hidden -WorkingDirectory $instance.WorkingDirectory | Out-Null }
        else { Start-Process -FilePath $instance.Path -WindowStyle Hidden | Out-Null }
    }
}
