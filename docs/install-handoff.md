# Installation handoff — 2026-10-07

## What this installs

DoubleTake sends the primary desktop and default playback audio from a Windows PC to an Apple TV's native AirPlay receiver. Install on the **sending PC**, not the TV. No companion TV app, WSL, VM or subscription is required.

| Device | Installation path / limit |
| --- | --- |
| Windows 10+ x64 PC | Native tray app and per-user installer; use a currently supported Windows edition. D3D11-capable graphics driver required. |
| Apple TV | Enable AirPlay; nothing to install. Prior user-confirmed picture/audio evidence is for AppleTV11,1, not every receiver. |
| Linux PC | Retained upstream CLI/Plasma source path; see [Linux build instructions](building.md#upstream-linux-reference). Not verified on Linux in this handoff. |
| macOS, Windows ARM64, phones/tablets, other smart TVs | No verified native installer/support claim in this fork. Do not assume the Windows package works on these devices. |

Sources: [requirements and compatibility](../README.md#requirements-and-current-limits), [packaging](../packaging/windows/README.md#install-upgrade-and-uninstall).

## Choose the right version

- Repository: <https://github.com/VelaguezSzpiech/doubletake-windows>, branch `main`.
- The published `windows-v1.0.0` release is the older installer. It **does not contain the current source changes**, including the later Full HD and diagnostic work. Do not use it to validate those changes.
- For the current changes, build the installer from the checked-out source using the next section. Building version `1.0.1` below labels a local build; it does not imply that a GitHub v1.0.1 release exists.
- Keep the installer, its `SHA256SUMS.txt`, and the exact source revision together when transferring a build to another PC. Never transfer installed settings, pairing keys or diagnostic archives.

## Build current source once, install on other Windows PCs

On a Windows x64 build machine, install **Git**, **Go 1.25.0 or newer**, a stable **.NET 8 SDK**, and **Inno Setup 6.7.3 or newer**. Internet is needed for uncached dependencies/notices. The build script requires the selected .NET SDK to be version 8; use `-DotnetPath` if another major SDK is the default. See [tool-path options](building.md#windows-prerequisites).

In PowerShell, from a directory where you want the source:

```powershell
git clone https://github.com/VelaguezSzpiech/doubletake-windows.git
Set-Location doubletake-windows
git rev-parse HEAD                   # record the source revision
go test ./...
.\scripts\build-windows.ps1 -Version 1.0.1
Get-Content .\dist\windows\SHA256SUMS.txt
Get-FileHash .\dist\windows\DoubleTake-Windows-1.0.1-Setup.exe -Algorithm SHA256
```

For an existing clean clone, use `git pull --ff-only` instead of cloning. Preserve any local edits first; do not force-reset a working tree.

Build output:

- `dist/windows/DoubleTake-Windows-1.0.1-Setup.exe`
- `dist/windows/SHA256SUMS.txt`
- `dist/windows/app/` — complete self-contained app payload, not just one executable.

The script recreates `dist/windows` and Release tray outputs. Save previous generated artifacts elsewhere first. It does not change the installed application or pairing. Details: [build outputs](building.md#windows-prerequisites).

On each destination Windows x64 PC:

1. Copy the installer and checksum file through a trusted channel. Compare `Get-FileHash .\DoubleTake-Windows-1.0.1-Setup.exe -Algorithm SHA256` with `SHA256SUMS.txt`. A checksum detects corruption; only a trusted source establishes provenance.
2. Quit an existing DoubleTake instance from its tray menu before upgrading. Run the installer interactively as the intended Windows user.
3. Follow the unsigned-app/SmartScreen policy for that PC. Do not disable security protections. Setup installs under `%LOCALAPPDATA%\Programs\DoubleTake`, without requesting elevation.
4. If a complete runtime is missing, Setup downloads and hash-verifies the official GStreamer **1.28.6 MSVC x86_64** prerequisite. This needs internet. End-user PCs need neither Go nor a .NET SDK/runtime installation; .NET is bundled.
5. Launch **DoubleTake** from the Start menu. There is no automatic startup or automatic connection. Launch independently of a terminal/agent session; do not use the diagnostic deployment script as the normal installer.

For restricted/offline environments, have the official complete GStreamer runtime installed through the approved software process first. This is not a fully offline bundled installer. Do not bypass a failed download/hash/runtime check. See [dependency handling](../packaging/windows/README.md#network-assisted-gstreamer-prerequisite).

## Connect and verify on each device

1. Enable Apple TV **Settings → AirPlay and HomeKit → AirPlay**. Put PC and TV on a trusted network permitting peer traffic; guest isolation may block it.
2. Select the intended default Windows playback device **before** connecting. Put a harmless test window on the primary monitor and play ordinary audio.
3. Open DoubleTake's notification-area icon (including hidden icons), then the settings gear. Select the TV and connect. If discovery fails, enter the TV's actual LAN address and AirPlay port (usually `7000`). Manual addressing does not bypass blocked VPN/LAN routes.
4. Enter the TV's onscreen PIN, or its configured fixed password, into the inline prompt. Pair separately on each PC; saved keys belong in that PC user's Windows Credential Manager.
5. Verify the actual TV picture and speaker sound. A connected indicator and sent-packet counters do **not** prove receiver playback. Check video and audio synchronization and run a longer normal-use session.
6. Hide/reopen the flyout; sharing should continue. Close the terminal/agent session that performed the build; a normally installed, independently launched tray app should not depend on it. Verify this on the destination PC.
7. Use **Disconnect**, then **Quit DoubleTake**. Reopen through Start and reconnect to check saved pairing.

No physical TV acceptance test is claimed by these instructions. Prior source documentation records a short successful AppleTV11,1/GTX 1650 session, but **long-run audio degradation, WSAENOBUFS/TCP aborts and occasional capture-probe stalls remain unresolved**. See [current limits](../README.md#verification-and-compatibility) and [troubleshooting](troubleshooting.md).

## Support, upgrades and removal

- Logs: gear → **Open log**, normally `%LOCALAPPDATA%\DoubleTake\state\doubletake.log`. Preferences are adjacent; pairing is in Credential Manager. Review/redact logs before sharing: they may contain network/device/path information.
- For no sound: check Windows/TV volumes and default playback routing, then disconnect/reconnect after output changes.
- For no picture/discovery: verify AirPlay, primary monitor, trusted LAN route, and required GStreamer plugins. Follow [troubleshooting](troubleshooting.md), not broad firewall/VPN disabling.
- Upgrade using a freshly built installer after quitting the tray. Uninstall through Windows Installed apps. Pairing/preferences and the separately installed GStreamer runtime are intentionally preserved.
- Diagnostic scripts are maintainer tools, not an installation prerequisite. Keep diagnostic output local; do not publish archives or credentials to GitHub.

Current source also writes `[DIAG]` records to `%LOCALAPPDATA%\DoubleTake\state\diag\diag-*.jsonl` by default. It retains up to six process archive files, each capped at roughly 500 MiB (the final record can exceed the cap). Set `DOUBLETAKE_DIAG_ARCHIVE=0` in the environment before launching the backend/tray to disable this archive; `[DIAG]` output in the ordinary log remains enabled. Do not share unreviewed archives. Source: `internal/airplay/diag_archive.go:13-64`.

## Verification recorded for this handoff

Observed on Windows x64 on 2026-10-07 (commands run from the repository root):

- `go test ./...`: PASS across all Go packages (Go 1.26.0); independent tester also ran `go vet ./cmd/doubletake ./internal/airplay ./internal/daemon`, PASS.
- `powershell -NoProfile -ExecutionPolicy Bypass -File scripts/build-windows.ps1 -Version 1.0.1 -DotnetPath <portable-dotnet-8>/dotnet.exe`: PASS, full self-contained payload and Inno Setup 6.7.3 installer produced. The PATH dotnet host had no SDK; explicitly selecting the existing .NET 8 SDK resolved this. Final build after the receiver-info privacy repair completed before 16:28 UTC.
- Generated installer SHA-256: `b722da2976ca050bacecfa41f0175c6ef94aaf97bc3dab3d63f58ed3b7c1d8d1`. This identifies this local build only; rebuilt installers can differ. Use the checksum shipped with your actual installer.
- Packaged backend smoke: `-h` printed options; final `-target 127.0.0.1 -port 1 -test -no-audio -creds <temporary-file>` with isolated state and `DOUBLETAKE_DIAG_ARCHIVE=0` emitted connection diagnostics, reported the expected refused connection and wrote no state files (16:28 UTC).
- `powershell -NoProfile -File scripts/audio-health.ps1 -Log <temporary-synthetic-log>`: PASS for a 91-minute healthy fixture (exit 0 / `PASS`), and correctly rejected failing/stale frames (exit 1 / `FAIL`). A Windows PowerShell 5.1 parse incompatibility in the optional `trends` field was fixed before these checks. Temporary fixtures were removed.
- Independent loopback receiver smoke reached pairing and FairPlay, with diagnostic archiving verified both enabled and disabled. Media startup stopped because the test environment did not discover a GStreamer H.264 encoder. This is **not** video/audio playback proof.
- After the privacy repair, `go test ./internal/airplay` passed independently; targeted `TestDiagSetupInfoPrivacyAcrossRecords`, `TestDiagRedactsKeyFieldsEverywhere` and `TestSessionNegotiatedRedactsKeyMaterial` passed. Synthetic binary/XML SETUP fixtures check that private receiver-info values do not leak across diagnostic record paths.
- Final scripts passed independent Windows PowerShell 5.1 and PowerShell 7 checks: equivalent raw JSONL/prefixed healthy fixtures, failing/stale rejection, and missing-start archive guidance. Deployment missing/incomplete payloads and an already-running tray were refused before launch; the existing tray remained alive. Root also exercised raw JSONL analysis and incomplete-payload rejection. Successful diagnostic launch and optional `-Eld` compilation were not exercised.
- Bounded independent re-review resolved all four publication findings: unrelated-process termination, missing diagnostic payload assumptions, archive format mismatch and nested receiver-info privacy filtering.
- Fresh-PC installation, physical Apple TV playback, long-duration audio soak, Linux and other platforms were **not exercised**. The per-device checklist above remains required.

The Go/source changes were already local before this handoff. This publication also adds these installation instructions and fixes the PowerShell helper compatibility issue. No new GitHub binary release is implied; `dist/` is generated and Git-ignored.

## Maintainer continuation

Start from this repository on `main`; read this handoff, [CHANGELOG.md](../CHANGELOG.md), and [building.md](building.md). First executable step is `git pull --ff-only` in a clean clone, then record `git rev-parse HEAD`. Build the current installer, install it on the target Windows x64 PC, and perform the physical picture/audio/long-run checklist above. Do not call the old v1.0.0 release current-source verification, and do not claim universal device support.

## Publication checkpoint

As of 2026-10-07, changes are committed locally, but the push was rejected because the active GitHub account lacks repository write access. No saved credential was available for the repository owner. Authenticate a write-authorized account, then run `git push origin main` from this checkout before expecting another device's clone to contain these changes. No credentials belong in this repository.
