<#
.SYNOPSIS
  Launches the tray and backend from a complete Windows app payload.

.DESCRIPTION
  Build the payload first with scripts\build-windows.ps1. The default is
  dist\windows\app; use -AppPath to select another complete payload directory.
  The directory must contain DoubleTake.Tray.exe, doubletake.exe, coreclr.dll,
  and PresentationFramework.dll. Quit DoubleTake yourself before running this
  script; it never kills processes, including GStreamer workloads.

  -Eld rebuilds the backend with AAC-ELD and therefore requires Go, the MSYS2
  UCRT64 GCC/pkgconf tools, and the fdk-aac pkg-config package. The tray and all
  other files still come from the validated complete app payload.

.EXAMPLE
  scripts\deploy-diagnostic.ps1
  scripts\deploy-diagnostic.ps1 -AppPath C:\build\dist\windows\app
  scripts\deploy-diagnostic.ps1 -Eld -AppPath C:\build\dist\windows\app
#>
[CmdletBinding()]
param(
    [Parameter()]
    [string]$AppPath = (Join-Path (Split-Path -Parent $PSScriptRoot) 'dist/windows/app'),
    # Experiment: override the tray's fixed video bitrate (kbps) for this run. 0 = unchanged.
    [int]$BitrateKbps = 0,
    [int]$Fps = 0,
    # Experiment: send AAC-ELD audio instead of ALAC (requires MSYS2 UCRT64 FDK-AAC).
    [switch]$Eld
)

$ErrorActionPreference = 'Stop'
$requiredFiles = @('DoubleTake.Tray.exe', 'doubletake.exe', 'coreclr.dll', 'PresentationFramework.dll')
$resolvedApp = (Resolve-Path -LiteralPath $AppPath -ErrorAction Stop).Path
foreach ($name in $requiredFiles) {
    if (-not (Test-Path -LiteralPath (Join-Path $resolvedApp $name) -PathType Leaf)) {
        throw "Incomplete app payload '$resolvedApp': missing $name. Build it with scripts\build-windows.ps1 first."
    }
}

$eldGcc = 'C:\msys64\ucrt64\bin\gcc.exe'
$eldPkgconf = 'C:\msys64\ucrt64\bin\pkgconf.exe'
if ($Eld) {
    $goCommand = Get-Command go.exe -ErrorAction SilentlyContinue
    if (-not $goCommand) { throw 'AAC-ELD requires go.exe on PATH.' }
    if (-not (Test-Path -LiteralPath $eldGcc -PathType Leaf)) { throw "AAC-ELD requires $eldGcc." }
    if (-not (Test-Path -LiteralPath $eldPkgconf -PathType Leaf)) { throw "AAC-ELD requires $eldPkgconf." }
    & $eldPkgconf --exists fdk-aac
    if ($LASTEXITCODE -ne 0) { throw 'AAC-ELD requires the fdk-aac package in MSYS2 UCRT64 pkg-config.' }
}

$runningApp = @(Get-Process -Name doubletake, DoubleTake.Tray -ErrorAction SilentlyContinue)
if ($runningApp.Count) {
    $names = ($runningApp | ForEach-Object { $_.ProcessName } | Sort-Object -Unique) -join ', '
    throw "DoubleTake is still running ($names). Quit the tray/backend yourself, then rerun this script. No processes were stopped."
}

if ($Eld) {
    $oldPath = $env:PATH
    $oldCgo = $env:CGO_ENABLED
    $oldCc = $env:CC
    $oldPkgConfig = $env:PKG_CONFIG
    try {
        $env:PATH = "C:\msys64\ucrt64\bin;$env:PATH"
        $env:CGO_ENABLED = '1'
        $env:CC = $eldGcc
        $env:PKG_CONFIG = $eldPkgconf
        Push-Location (Split-Path -Parent $PSScriptRoot)
        try {
            & $goCommand.Source build -trimpath -buildvcs=false -tags fdk_aac '-ldflags=-s -w -extldflags=-static' -o (Join-Path $resolvedApp 'doubletake.exe') .\cmd\doubletake
            if ($LASTEXITCODE -ne 0) { throw "AAC-ELD backend build failed with exit $LASTEXITCODE." }
        } finally { Pop-Location }
    } finally {
        $env:PATH = $oldPath
        $env:CGO_ENABLED = $oldCgo
        $env:CC = $oldCc
        $env:PKG_CONFIG = $oldPkgConfig
    }
}

Remove-Item Env:DOUBLETAKE_TARGET_LATENCY_MS -ErrorAction SilentlyContinue
Remove-Item Env:DOUBLETAKE_BITRATE_KBPS, Env:DOUBLETAKE_FPS, Env:DOUBLETAKE_AUDIO_CODEC -ErrorAction SilentlyContinue
if ($Eld) { $env:DOUBLETAKE_AUDIO_CODEC = 'aac-eld' }
if ($BitrateKbps -gt 0) { $env:DOUBLETAKE_BITRATE_KBPS = "$BitrateKbps" }
if ($Fps -gt 0) { $env:DOUBLETAKE_FPS = "$Fps" }
Start-Process -FilePath (Join-Path $resolvedApp 'DoubleTake.Tray.exe') -WorkingDirectory $resolvedApp
Start-Sleep 30
Get-Date -Format o
Get-Process doubletake, DoubleTake.Tray -ErrorAction SilentlyContinue | Select-Object Id, ProcessName, StartTime | Format-Table -AutoSize
$log = "$env:LOCALAPPDATA\DoubleTake\state\doubletake.log"
if (Test-Path -LiteralPath $log) {
    $kinds = @{}
    foreach ($line in [System.IO.File]::ReadLines($log)) {
        $m = [regex]::Match($line, '\[DIAG\] \{.*?"kind":"([a-z_.]+)"')
        if ($m.Success) { $kinds[$m.Groups[1].Value] = 1 + [int]$kinds[$m.Groups[1].Value] }
    }
    $kinds.GetEnumerator() | Sort-Object Value -Descending | Select-Object Value, Name | Format-Table -AutoSize
    "log size: {0:N1} MB" -f ((Get-Item $log).Length / 1MB)
}
