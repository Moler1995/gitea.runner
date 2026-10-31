# Runs the test binaries after scripts/test-windows-setup.ps1, reports to the VM's shared folder.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$results = '\\host.lan\Data'
$root = $PSScriptRoot
try {
    New-Item -ItemType Directory "$root\logs" | Out-Null
    $env:Path = "C:\git\cmd;C:\git\bin;C:\pwsh;C:\node;$env:Path"

    $failed = 0
    Write-Output "==> Running $((Get-Content "$root\tests.txt").Count) test binaries on $([Environment]::ProcessorCount) cores"
    Get-Content "$root\tests.txt" | ForEach-Object -ThrottleLimit ([Environment]::ProcessorCount) -Parallel {
        $binary, $package = $_ -split ' ', 2
        $started = Get-Date
        Set-Location "$using:root\src\$package"
        cmd.exe /d /c "$using:root\bin\$binary -test.v -test.timeout=60m -test.parallel=8 > $using:root\logs\$($package -replace '/', '-').log 2>&1"
        [pscustomobject]@{ Package = $package; ExitCode = $LASTEXITCODE; Seconds = [int]((Get-Date) - $started).TotalSeconds }
    } | ForEach-Object {
        $result = 'ok'
        if ($_.ExitCode -ne 0) {
            $result = 'FAIL'
            $failed = 1
        }
        Add-Content "$results\summary.txt" "$result $($_.Package) $($_.Seconds)s"
    }
    Copy-Item "$root\logs\*" $results
} catch {
    $_ | Out-String | Tee-Object "$results\error.txt"
    $failed = 1
}
Set-Content "$results\exitcode" $failed
