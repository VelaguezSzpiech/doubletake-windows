# Windows release packaging

This is an **unofficial** Windows port of [Omar Roth's DoubleTake](https://github.com/omarroth/doubletake), maintained at <https://github.com/VelaguezSzpiech/doubletake-windows>. The original LGPLv3 `LICENSE`, accompanying `COPYING.GPL`, attribution, and complete source remain available. See `THIRD_PARTY_NOTICES.md` and the generated payload's `licenses/` directory for dependency notices.

## Build

Build on x64 Windows 10 or newer using:

- A stable Go toolchain at least as new as the `go` directive in `go.mod`.
- A stable **.NET 8 SDK**. The release bundles the runtime; end users do not need this SDK or a separately installed .NET runtime.
- **Inno Setup 6.7.3 or newer** for installer builds. No paid packaging tools are required.
- Internet access for uncached Go/.NET dependencies and for WPF/Windows Forms notices fetched from immutable source revisions matching the selected runtime pack.

From the source root, in PowerShell:

```powershell
.\scripts\build-windows.ps1 -Version 1.0.0
```

Optional arguments:

```powershell
.\scripts\build-windows.ps1 -Version 1.0.0 -DotnetPath 'D:\Tools\dotnet\dotnet.exe' -ISCCPath 'D:\Tools\Inno Setup 6\ISCC.exe'
.\scripts\build-windows.ps1 -Version 1.0.0 -SkipInstaller
```

`-DotnetPath` and `-ISCCPath` accept executable file paths, not directories. Without them, executables are resolved from `PATH`; Inno Setup is also searched for in its standard per-user and Program Files installation locations. `-SkipInstaller` requires no Inno Setup compiler. The script can be invoked from any working directory.

`-Version` defaults to `1.0.0`. It must be `major.minor.patch`, with each component in `0..65535`, no leading zeroes except `0` itself, and no prerelease/build suffixes. This version is used in WPF version metadata, installer metadata, and the installer filename.

### Exact output and cleanup behavior

The script fails on missing tools, incompatible toolchain versions, incomplete licensing, or nonzero external-command exit codes. It recreates `dist/windows` and cleans the tray's Release/win-x64 build outputs before publishing; existing releases in that generated directory are replaced. Reparse points in that generated tree or its `dist` parent are rejected rather than followed. It does not delete installed applications, source, user settings, or credentials. Normal toolchain restore/build caches may be updated.

The payload at `dist/windows/app/` contains:

- `DoubleTake.Tray.exe`: the self-contained `win-x64` WPF Release frontend.
- `doubletake.exe`: the adjacent Go Windows/amd64 backend, built with `-trimpath`, VCS stamping disabled, and debug symbol stripping.
- The resolved .NET runtime, including Windows Desktop/WPF runtime files.
- Source `LICENSE`, `COPYING.GPL`, and `THIRD_PARTY_NOTICES.md`.
- Actual Go module license/notice files resolved through Go module metadata, the Go standard-library license, and license texts from the referenced .NET runtime packs. Missing Desktop component notices are fetched from the exact WPF/Windows Forms source revisions identified by runtime-pack provenance, with source URLs retained. `licenses/INDEX.txt` records dependency identifiers and versions, **not** local module-cache paths.

PDB/MDB debug symbols are not distributed. Clean compilation and compiler path mapping prevent reuse of assemblies containing local PDB paths. The installer excludes logs and dumps. Installed settings, pairings, or private diagnostics are never copied into the payload. Local/replaced Go modules are rejected for release builds. The script does not run tests, lint, or formatters; run the project's verification separately before publishing.

The default build additionally creates:

```text
dist/windows/DoubleTake-Windows-1.0.0-Setup.exe
dist/windows/SHA256SUMS.txt
```

`SHA256SUMS.txt` contains the installer SHA-256 and relative filename in conventional two-space format. No portable archive is generated, and `-SkipInstaller` produces only the app payload (no installer or checksum file). `dist/windows/dependencies.iss` is a generated compiler input derived from `dependencies.json`; it is not installed. The source `dist/` tree is ignored by Git.

## Network-assisted GStreamer prerequisite

The installer is **network-assisted**, not completely offline. GStreamer is external to the app payload and is installed through its official user installer only when a complete runtime is missing. No Go SDK, .NET SDK, or paid runtime is needed by end users.

The pinned dependency in `dependencies.json` is GStreamer **1.28.6**, MSVC x86-64:

- URL: <https://gstreamer.freedesktop.org/data/pkg/windows/1.28.6/msvc/gstreamer-1.0-msvc-x86_64-1.28.6.exe>
- SHA-256: `059251444d1267b486eba390b18d25fed87e10315e72f757ec6c7e912fa746b5`

Detection follows the application's runtime search order:

1. `GSTREAMER_1_0_ROOT_MSVC_X86_64` (if set).
2. `%LOCALAPPDATA%\Programs\gstreamer\1.0\msvc_x86_64`.
3. `%ProgramFiles%\gstreamer\1.0\msvc_x86_64`.
4. The conventional `C:\gstreamer\1.0\msvc_x86_64` directory.

An existing runtime is accepted only when both launch/inspection executables, the core runtime DLL, and all required plugin DLLs listed in `dependencies.json` are present. These include D3D11 desktop capture, NVENC, WASAPI2 loopback, conversion/resampling, OpenH264, video parsing, RTP, and ONVIF timestamp support. Presence does not guarantee that a particular GPU supports NVENC; the application still performs its own element/hardware checks.

If no complete runtime is found, Setup displays download progress and uses Inno Setup's HTTPS download support with the pinned SHA-256. It verifies the hash again immediately before executing the official installer with `/CURRENTUSER /VERYSILENT /SUPPRESSMSGBOXES /NORESTART`, checks its exit code, and checks the installed runtime again. Download failure, hash mismatch, unsuccessful child installation, or an incomplete resulting runtime stops DoubleTake installation with a visible error; it is never treated as success. An incomplete higher-priority runtime or environment override must be repaired or removed if it shadows the installed per-user runtime. HTTPS certificate validation is not weakened.

GStreamer supplies its own license texts and notices through its official installation. It is not bundled in the DoubleTake installer payload.

## Install, upgrade, and uninstall

- Windows 10 or newer, x64-compatible OS; the application payload is `win-x64`.
- Per-user install, no elevation requested, default `%LOCALAPPDATA%\Programs\DoubleTake`.
- Stable installer AppId `{57A967D4-A798-4A17-8A49-AE50A82E71F3}` supports in-place upgrades.
- A Start menu shortcut points directly to the GUI executable and supports taskbar pinning; a desktop shortcut is optional and unchecked by default.
- No scheduled task, service, Run-key entry, or autostart shortcut is created.
- Optional post-install launch is unchecked by default, omitted on silent installs, and supplies no receiver/connection arguments. Launching DoubleTake does not automatically connect to a receiver.
- Upgrades use Windows Restart Manager to ask to close applications using the payload. Automatic restarting is disabled. If the frontend/backend remains locked or cannot be updated, Setup stops before copying application files with a clear instruction to quit from the tray menu and retry. Forced process termination is not requested.
- Windows Settings / Installed apps receives a real uninstall entry. Quit DoubleTake before uninstalling; uninstall refuses when the frontend/backend cannot be updated rather than silently leaving a running installation.
- Uninstall removes only files/shortcuts owned by the app installation. Settings/state outside the app installation and saved pairing in Windows Credential Manager are preserved on both upgrade and uninstall. The separately installed GStreamer runtime is also preserved and can be removed separately through its own uninstall entry.

## Signature status

The DoubleTake installer and application are **unsigned**. Windows may show a SmartScreen reputation warning or an unknown-publisher prompt. Verify the release source and published SHA-256 before deciding whether to run it; organizational policy may block unsigned software. Do not disable Windows security protections to install it. This build script neither signs artifacts nor bypasses signature/security checks.
