$ErrorActionPreference = 'Stop'

$Root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$Source = Join-Path $Root 'dist\windows-x86_64'
$Destination = Join-Path $env:LOCALAPPDATA 'AGS\AndroidCapture'
if (-not (Test-Path (Join-Path $Source 'ags-android-capture.exe'))) {
    throw 'Build the Windows bundle first using scripts/build-windows.sh in MSYS2 MINGW64.'
}

New-Item -ItemType Directory -Force -Path $Destination | Out-Null
Copy-Item -Path (Join-Path $Source '*') -Destination $Destination -Recurse -Force

$StartScript = Join-Path $Destination 'start-ags-capture.ps1'
@'
$Executable = Join-Path $PSScriptRoot 'ags-android-capture.exe'
$Existing = Get-CimInstance Win32_Process -Filter "name = 'ags-android-capture.exe'" | Where-Object { $_.CommandLine -like '*serve*127.0.0.1:17653*' }
if (-not $Existing) {
    Start-Process -FilePath $Executable -ArgumentList @('serve', '--listen', '127.0.0.1:17653', '--idle-timeout=-1s') -WindowStyle Hidden
}
'@ | Set-Content -Encoding UTF8 $StartScript

$TaskAction = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -ExecutionPolicy Bypass -File `"$StartScript`""
$TaskTrigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
Register-ScheduledTask -TaskName 'AGS Android Capture' -Action $TaskAction -Trigger $TaskTrigger -Description 'Starts the local AGS browser bridge; adb and scrcpy run only when a stream is requested.' -Force | Out-Null
Start-ScheduledTask -TaskName 'AGS Android Capture'
Write-Host "Installed at $Destination. The bridge listens on loopback; each ADB/scrcpy session starts only after the AGS page requests it."
