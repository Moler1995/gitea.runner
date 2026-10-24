# Runs at the logon of a test VM from scripts/test-windows.sh, reports to its shared folder.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$results = '\\host.lan\Data\results'
$root = 'C:\tests'
try {
    Set-Content "$results\started" ''
    robocopy \\host.lan\Data\bundle $root /E /MT:8 /NP /NFL /NDL /NJH /NJS | Out-Null
    if ($LASTEXITCODE -ge 8) { throw "copying the tests failed with exit code $LASTEXITCODE" }
    New-Item -ItemType Directory "$root\logs" | Out-Null
    $env:Path = "C:\git\cmd;C:\git\bin;C:\pwsh;C:\node;$env:Path"

    $failed = 0
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
    $_ | Out-String | Set-Content "$results\error.txt"
    $failed = 1
}
Set-Content "$results\exitcode" $failed
Start-Sleep -Seconds 600 # the host normally removes the VM first
Stop-Computer -Force
