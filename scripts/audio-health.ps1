<#
.SYNOPSIS
  Summarizes audio diagnostics from the DoubleTake log or JSONL archive.

.DESCRIPTION
  Reads the newest audio session (from its last "audio.start" record) and prints a
  machine-readable JSON verdict: duration, status counts, every non-ok interval,
  every event, source-age/clock-correction trends and a soak PASS/FAIL.

  -Log accepts either ordinary log lines containing [DIAG] records or a raw
  diag-*.jsonl archive. Supply one file explicitly; files are not merged. If a
  rotated ordinary log has no audio.start, use the session archive because the
  tray log may have rotated away the start record.

  A session PASSES the soak only if it ran at least -MinSoakMinutes, never reported
  a "failing" interval, never dropped stale frames, and has recent health records
  (a log that went quiet is a failure, not a pass). A pass is necessary but not
  sufficient: it says nothing about what the receiver actually played.

.EXAMPLE
  scripts\audio-health.ps1
  scripts\audio-health.ps1 -MinSoakMinutes 90 -Log C:\path\doubletake.log
  scripts\audio-health.ps1 -Log "$env:LOCALAPPDATA\DoubleTake\state\diag\diag-20261007-120000-1234.jsonl"
#>
param(
    [string]$Log = "$env:LOCALAPPDATA\DoubleTake\state\doubletake.log",
    [int]$MinSoakMinutes = 90,
    [int]$MaxQuietSeconds = 20
)

$ErrorActionPreference = 'Stop'
$records = New-Object System.Collections.Generic.List[object]
foreach ($line in [System.IO.File]::ReadLines($Log)) {
    $at = $line.IndexOf('[DIAG] ')
    if ($at -ge 0) {
        $json = $line.Substring($at + 7)
    } elseif ($line.TrimStart().StartsWith('{')) {
        $json = $line
    } else {
        continue
    }
    try { $records.Add(($json | ConvertFrom-Json)) } catch { }
}

$startIndex = -1
for ($i = $records.Count - 1; $i -ge 0; $i--) {
    if ($records[$i].kind -eq 'audio.start') { $startIndex = $i; break }
}
if ($startIndex -lt 0) {
    [pscustomobject]@{ verdict = 'NO_SESSION'; reason = "no audio.start record in $Log. If this tray log has rotated, select the session archive in %LOCALAPPDATA%\DoubleTake\state\diag with -Log; log and archive files are not merged automatically." } | ConvertTo-Json
    exit 2
}

$session = $records.GetRange($startIndex, $records.Count - $startIndex)
$start = $session[0]
$health = @($session | Where-Object kind -eq 'audio.health')
$events = @($session | Where-Object { $_.kind -ne 'audio.health' -and $_.kind -ne 'audio.start' })
$stop = $session | Where-Object kind -eq 'audio.stop' | Select-Object -Last 1
$bad = @($health | Where-Object status -ne 'ok')
$lastHealth = $health | Select-Object -Last 1

# ConvertFrom-Json may already have turned ISO timestamps into DateTime.
function ConvertTo-Offset($value) {
    if ($value -is [datetime]) { return [datetimeoffset]$value }
    return [datetimeoffset]::Parse([string]$value, [cultureinfo]::InvariantCulture)
}
$startedAt = ConvertTo-Offset $start.ts
$lastAt = if ($lastHealth) { ConvertTo-Offset $lastHealth.ts } else { $startedAt }
$minutes = [math]::Round(($lastAt - $startedAt).TotalMinutes, 1)
$quietSeconds = if ($stop) { 0 } else { [math]::Round(([datetimeoffset]::Now - $lastAt).TotalSeconds) }

$flagCounts = @{}
foreach ($h in $bad) { foreach ($f in $h.flags) { $flagCounts[$f] = 1 + [int]$flagCounts[$f] } }

$failures = @()
if ($minutes -lt $MinSoakMinutes) { $failures += "ran $minutes min, soak needs $MinSoakMinutes" }
if ($stop) { $failures += "session ended: $($stop.error)" }
elseif ($quietSeconds -gt $MaxQuietSeconds) { $failures += "no health record for $quietSeconds s (stalled loop or dead process)" }
if (@($health | Where-Object status -eq 'failing').Count) { $failures += 'failing intervals present' }
if ($lastHealth -and $lastHealth.frames_stale_total -gt 0) { $failures += "stale frames dropped: $($lastHealth.frames_stale_total)" }

$samples = @($health | Where-Object { $_.source_age_us })
[pscustomobject]@{
    verdict         = if ($failures.Count) { 'FAIL' } else { 'PASS' }
    failures        = $failures
    session_started = $start.ts
    last_health     = if ($lastHealth) { $lastHealth.ts } else { $null }
    minutes         = $minutes
    ended           = [bool]$stop
    health_records  = $health.Count
    status_counts   = [pscustomobject]@{
        ok       = @($health | Where-Object status -eq 'ok').Count
        degraded = @($health | Where-Object status -eq 'degraded').Count
        failing  = @($health | Where-Object status -eq 'failing').Count
    }
    flag_counts     = $flagCounts
    trends          = if ($samples.Count) {
        [pscustomobject]@{
            source_age_mean_us_first   = $samples[0].source_age_us.mean
            source_age_mean_us_last    = $samples[-1].source_age_us.mean
            source_age_max_us_overall  = ($samples | ForEach-Object { $_.source_age_us.max } | Measure-Object -Maximum).Maximum
            min_age_headroom_us        = ($samples | ForEach-Object { $_.age_headroom_us } | Measure-Object -Minimum).Minimum
            clock_correction_us_last   = $lastHealth.clock_correction_us
            drift_ppm_mean             = [math]::Round(($health | Where-Object { $null -ne $_.drift_ppm } | Measure-Object drift_ppm -Average).Average, 1)
            frames_sent_total          = $lastHealth.frames_sent_total
            retransmit_requests_total  = $lastHealth.retransmit.requests_total
            wsaenobufs_retries_total   = $lastHealth.wsaenobufs_retries_total
        }
    } else { $null }
    non_ok_intervals = @($bad | Select-Object -First 40 ts, status, flags, frames_sent_iv, frames_stale_iv, send_max_us_iv, read_wait_max_us_iv, age_headroom_us, drift_ppm, receiver_feedback)
    events           = @($events | Select-Object -First 80)
    latest_health    = $lastHealth
} | ConvertTo-Json -Depth 8
if ($failures.Count) { exit 1 }
