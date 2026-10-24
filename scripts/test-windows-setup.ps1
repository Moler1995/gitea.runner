# Runs once at the first logon of a new base image from scripts/test-windows.sh, then powers it off.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$shared = '\\host.lan\Data'
try {
    if (Get-Command Set-MpPreference -ErrorAction SilentlyContinue) {
        Set-MpPreference -DisableRealtimeMonitoring $true # scanning every spawned test process costs minutes
        Add-MpPreference -ExclusionPath C:\
    }
    Set-ItemProperty HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem LongPathsEnabled 1
    foreach ($tool in @(
            @('git.7z.exe', 'https://github.com/git-for-windows/git/releases/download/v2.56.0.windows.3/PortableGit-2.56.0.3-64-bit.7z.exe', '4fc27b779829a36df77911e3f4dea09c66c790dd6c5f7dfa3b6a5479f1fe354f'),
            @('node.zip', 'https://nodejs.org/dist/v24.22.1/node-v24.22.1-win-x64.zip', 'd2046d28c2c3dcd211ac5145c0bf30e2047ef9dfb306693c8c6e2eccd8b95fa5'),
            @('pwsh.zip', 'https://github.com/PowerShell/PowerShell/releases/download/v7.6.8/PowerShell-7.6.8-win-x64.zip', 'b9f074f923c499b51ec99d313da8e9ae90d17a9f3adf00c73d6189f34d908528'))) {
        Invoke-WebRequest -UseBasicParsing $tool[1] -OutFile "$env:TEMP\$($tool[0])"
        if ((Get-FileHash "$env:TEMP\$($tool[0])").Hash -ne $tool[2]) { throw "checksum mismatch for $($tool[1])" }
    }
    Start-Process -FilePath "$env:TEMP\git.7z.exe" -ArgumentList '-oC:\git', '-y' -Wait
    tar.exe -xf "$env:TEMP\node.zip" -C C:\
    Rename-Item (Resolve-Path C:\node-*-win-x64).Path C:\node
    New-Item -ItemType Directory C:\pwsh | Out-Null
    tar.exe -xf "$env:TEMP\pwsh.zip" -C C:\pwsh
    $env:Path = "C:\git\cmd;C:\git\bin;C:\pwsh;C:\node;$env:Path"
    Get-Command git, bash, node, pwsh | Out-Null

    $script = "$shared\test-windows.ps1"
    Register-ScheduledTask -TaskName gitea-runner-tests `
        -Trigger (New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME) `
        -Principal (New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Highest) `
        -Action (New-ScheduledTaskAction -Execute C:\pwsh\pwsh.exe -Argument "-NoProfile -ExecutionPolicy Bypass -Command `"while (-not (Test-Path $script)) { Start-Sleep -Seconds 2 }; & $script`"") | Out-Null
    Remove-Item "$env:TEMP\git.7z.exe", "$env:TEMP\node.zip", "$env:TEMP\pwsh.zip"
    Set-Content "$shared\ready" ''
} catch {
    $_ | Out-String | Set-Content "$shared\error.txt"
}
Stop-Computer -Force
