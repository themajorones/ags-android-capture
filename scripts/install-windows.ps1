$ErrorActionPreference = 'Stop'

trap {
    Write-Host ''
    Write-Host 'AGS Android Capture installation failed:' -ForegroundColor Red
    Write-Host ($_ | Format-List * -Force | Out-String) -ForegroundColor Red
    Read-Host 'Press Enter to close this window'
    exit 1
}

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
if (Test-Path (Join-Path $ScriptDir 'ags-android-capture.exe')) {
    $Source = $ScriptDir
} else {
    $Root = Split-Path -Parent $ScriptDir
    $Source = Join-Path $Root 'dist\windows-x86_64'
}
$Destination = Join-Path $env:LOCALAPPDATA 'AGS\AndroidCapture'
if (-not (Test-Path (Join-Path $Source 'ags-android-capture.exe'))) {
    throw 'Build or download the complete Windows bundle first. Local builds use the MSYS2 UCRT64 or MINGW64 environment.'
}
$SystemADB = Get-Command adb -ErrorAction SilentlyContinue
if (-not $SystemADB) {
    throw 'Missing runtime requirement: system ADB. Install Android Platform Tools and add adb.exe to PATH, then run this installer again.'
}
& $SystemADB.Source version | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw "System ADB was found at '$($SystemADB.Source)' but could not start. Repair Android Platform Tools before installing the capture bridge."
}

$ScrcpyPath = Join-Path $Source 'scrcpy.exe'
foreach ($Pattern in @('SDL3.dll', 'avcodec-*.dll', 'avformat-*.dll', 'avutil-*.dll')) {
    if (-not (Get-ChildItem -Path $Source -Filter $Pattern -File -ErrorAction SilentlyContinue)) {
        throw "The Windows bundle is incomplete: scrcpy runtime dependency '$Pattern' is missing. Download or rebuild the complete bundle."
    }
}
try {
    $ScrcpyVersionOutput = (& $ScrcpyPath --version 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0) {
        throw $ScrcpyVersionOutput.Trim()
    }
} catch {
    throw "scrcpy cannot start from this bundle. Its Windows runtime dependencies are incomplete or incompatible: $($_.Exception.Message)"
}

New-Item -ItemType Directory -Force -Path $Destination | Out-Null
$InstalledExecutable = [System.IO.Path]::GetFullPath((Join-Path $Destination 'ags-android-capture.exe'))

# The per-user scheduled task keeps the bridge process alive, which locks its
# executable on Windows. Stop both the task and only the bridge process from
# this install before replacing the bundle. Kill the process tree so an active
# scrcpy child cannot be left behind; the shared system ADB server is separate
# and is intentionally left running.
$CaptureTask = Get-ScheduledTask -TaskName 'AGS Android Capture' -ErrorAction SilentlyContinue
if ($CaptureTask -and $CaptureTask.State -eq 'Running') {
    Stop-ScheduledTask -TaskName 'AGS Android Capture'
    $TaskStopDeadline = (Get-Date).AddSeconds(10)
    do {
        Start-Sleep -Milliseconds 200
        $CaptureTask = Get-ScheduledTask -TaskName 'AGS Android Capture' -ErrorAction SilentlyContinue
    } while ($CaptureTask -and $CaptureTask.State -eq 'Running' -and (Get-Date) -lt $TaskStopDeadline)
}

$InstalledProcesses = Get-CimInstance Win32_Process -Filter "name = 'ags-android-capture.exe'" |
    Where-Object {
        $_.ExecutablePath -and
        [string]::Equals([System.IO.Path]::GetFullPath($_.ExecutablePath), $InstalledExecutable,
            [System.StringComparison]::OrdinalIgnoreCase)
    }
foreach ($CaptureProcess in $InstalledProcesses) {
    & taskkill.exe /PID $CaptureProcess.ProcessId /T /F | Out-Null
    $ProcessStopDeadline = (Get-Date).AddSeconds(10)
    do {
        Start-Sleep -Milliseconds 200
        $StillRunning = Get-CimInstance Win32_Process -Filter "ProcessId = $($CaptureProcess.ProcessId)" -ErrorAction SilentlyContinue
    } while ($StillRunning -and (Get-Date) -lt $ProcessStopDeadline)
    if ($StillRunning) {
        throw "Could not stop the installed capture bridge (PID $($CaptureProcess.ProcessId)); close it before retrying installation."
    }
}

Get-ChildItem -Path $Source -File | Where-Object { $_.Name -notin @('install.ps1', 'README.md') } | Copy-Item -Destination $Destination -Force
Remove-Item -Path (Join-Path $Destination 'adb.exe'), (Join-Path $Destination 'AdbWinApi.dll'), (Join-Path $Destination 'AdbWinUsbApi.dll') -Force -ErrorAction SilentlyContinue

$StartScript = Join-Path $Destination 'start-ags-capture.ps1'
$AdbPathLiteral = $SystemADB.Source.Replace("'", "''")
$StartScriptContent = @'
$env:AGS_CAPTURE_ADB = '__SYSTEM_ADB_PATH__'
$Executable = Join-Path $PSScriptRoot 'ags-android-capture.exe'
$Existing = Get-CimInstance Win32_Process -Filter "name = 'ags-android-capture.exe'" | Where-Object { $_.CommandLine -like '*serve*127.0.0.1:17653*' }
if (-not $Existing) {
    $Stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    $Stdout = Join-Path $PSScriptRoot "capture-$Stamp.stdout.log"
    $Stderr = Join-Path $PSScriptRoot "capture-$Stamp.stderr.log"
    Start-Process -FilePath $Executable -ArgumentList @('serve', '--listen', '127.0.0.1:17653', '--idle-timeout=-1s') -RedirectStandardOutput $Stdout -RedirectStandardError $Stderr -WindowStyle Hidden
}
'@
$StartScriptContent = $StartScriptContent.Replace('__SYSTEM_ADB_PATH__', $AdbPathLiteral)
$StartScriptContent | Set-Content -Encoding UTF8 $StartScript

$TaskAction = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -ExecutionPolicy Bypass -File `"$StartScript`""
$TaskTrigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
Register-ScheduledTask -TaskName 'AGS Android Capture' -Action $TaskAction -Trigger $TaskTrigger -Description 'Starts the local AGS browser bridge; adb and scrcpy run only when a stream is requested.' -Force | Out-Null
Start-ScheduledTask -TaskName 'AGS Android Capture'
Write-Host "Installed at $Destination. The bridge uses system ADB at $($SystemADB.Source) and listens on loopback; each ADB/scrcpy session starts only after the AGS page requests it."
