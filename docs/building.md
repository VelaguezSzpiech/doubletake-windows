# Building and testing

This repository preserves the upstream Go sender and Linux tools and adds a native Windows WPF tray and installer. Windows packaging does not use WSL, bundle a Linux VM or install software on the Apple TV.

## Windows prerequisites

Build on Windows x64 with:

- **Go 1.25.0 or newer**, as specified by [`go.mod`](../go.mod).
- **.NET 8 SDK**, with Windows desktop targeting support. The end-user .NET runtime is included by self-contained publishing.
- **Inno Setup 6.7.3 or newer**, needed for the default installer build, not for `-SkipInstaller`.
- Internet access for uncached Go/.NET dependencies and WPF/Windows Forms notices fetched from immutable source revisions matching the runtime. GStreamer is an external runtime prerequisite for capture and playback testing, not committed to this repository.

The build script uses standard tool discovery; use the explicit executable-path options below if your SDK/compiler is not on PATH. From the repository root in PowerShell:

```powershell
.\scripts\build-windows.ps1 -Version 1.0.0
```

This builds the Go backend and self-contained `win-x64` WPF application, collects license/notices, and compiles the installer. Output:

The script recreates `dist/windows` and cleans the tray's Release/win-x64 build outputs to avoid stale debug metadata. Save any generated artifacts you need elsewhere before rebuilding; installed applications, user state and pairing are not removed. `-Version` defaults to `1.0.0` and accepts numeric `major.minor.patch` release versions, not prerelease suffixes.

| Path | Contents |
| --- | --- |
| `dist/windows/app/` | `DoubleTake.Tray.exe`, adjacent `doubletake.exe`, bundled .NET runtime and license/notices; no PDBs or private diagnostic logs |
| `dist/windows/DoubleTake-Windows-1.0.0-Setup.exe` | Unsigned per-user Windows installer |
| `dist/windows/SHA256SUMS.txt` | SHA-256 checksum of the installer |

Optional flags:

```powershell
# Build the complete application payload without compiling an installer.
.\scripts\build-windows.ps1 -Version 1.0.0 -SkipInstaller

# Supply executable files, not directories. Replace these illustrative paths.
.\scripts\build-windows.ps1 -Version 1.0.0 `
  -DotnetPath 'C:\Tools\dotnet\dotnet.exe' `
  -ISCCPath 'C:\Tools\Inno Setup 6\ISCC.exe'
```

`-SkipInstaller` produces the app directory only, not an installer/checksum or a portable archive. To run that payload, install the complete 64-bit MSVC GStreamer runtime yourself and launch `dist/windows/app/DoubleTake.Tray.exe`; keep the entire publish directory together. End users should normally use the installer, which handles the external prerequisite.

### GStreamer prerequisite

The installer detects existing runtime executables and required plugin files in normal per-user and machine-wide locations. If a complete runtime is absent it downloads the official **1.28.6 MSVC x86_64** user installer:

- URL: <https://gstreamer.freedesktop.org/data/pkg/windows/1.28.6/msvc/gstreamer-1.0-msvc-x86_64-1.28.6.exe>
- SHA-256: `059251444d1267b486eba390b18d25fed87e10315e72f757ec6c7e912fa746b5`

It verifies this hash **before execution**, runs the prerequisite per-user, and checks runtime completeness afterward. This is a network-assisted installer, not a fully offline bundle. Download, verification or installation failures abort setup visibly. Do not remove the hash check or substitute an unverified download. Existing GStreamer is not removed by DoubleTake uninstall.

For source builds/custom runtime locations, `GSTREAMER_1_0_ROOT_MSVC_X86_64` can point to the runtime root containing `bin`. The tray also searches the normal user/machine locations and prepends the discovered runtime to its child processes' PATH. See [troubleshooting](troubleshooting.md#gstreamer-or-encoder-errors) for element checks.

### Tests and manual verification

From the repository root:

```powershell
go test ./...
```

This is the Go test-suite command, not proof of physical receiver compatibility or a UI test. The WPF project has no separate test-suite command; publishing it is part of the build script. Capture tests require a real interactive desktop and GStreamer; an automated protocol test receiver cannot prove visual output or speaker audibility.

After building, launch the actual tray and verify:

1. Single-click toggles the flyout; Escape/outside click hides it without disconnecting.
2. Receiver selection and manual IP work, including your LAN/VPN route configuration.
3. Pairing prompts work inline; reconnect uses saved credentials.
4. The TV visibly displays the primary desktop. Separately check whether playback audio is audible at the TV/speakers.
5. Disconnect/Quit stops sharing and capture subprocesses; launching again does not create a duplicate tray instance.
6. Install/upgrade/uninstall works for a normal user, preserving state and pairing. Check optional shortcuts and the uninstall entry.

These are verification steps for your build, not claims that every setup has already passed. The [README](../README.md#verification-and-compatibility) distinguishes observed Windows evidence from upstream reports.

## Upstream Linux reference

The following retains useful upstream build and operation guidance. Linux options are not Windows flyout features. Refer also to the retained [man pages](../man/man1/) and [KDE Plasma widget documentation](../plasmoid/README.md).

### Linux requirements and packages

Use the Go version in `go.mod` (currently 1.25.0 or newer), GStreamer 1.0 with Base/Good/Bad/Ugly and libav plugins, PulseAudio utilities (`pactl`), and PipeWire/xdg-desktop-portal on Wayland or X11 for screen capture.

Ubuntu/Debian:

```sh
sudo apt install libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev \
  gstreamer1.0-plugins-base gstreamer1.0-plugins-good gstreamer1.0-plugins-bad \
  gstreamer1.0-plugins-ugly gstreamer1.0-libav gstreamer1.0-x pulseaudio-utils
```

The synthetic video tests also require `timeoverlay`, supplied by `gstreamer1.0-x` on Ubuntu/Debian. Without it, capture tests can end with EOF even when the encoder and RTP plugins are installed.

Arch Linux:

```sh
sudo pacman -S gstreamer gst-plugins-base gst-plugins-good gst-plugins-bad \
  gst-plugins-ugly gst-libav libpulse
```

`openh264enc` normally comes from Bad Plug-ins; check with `gst-inspect-1.0 openh264enc`. Upstream AUR packages remain [doubletake](https://aur.archlinux.org/packages/doubletake), [doubletake-git](https://aur.archlinux.org/packages/doubletake-git), and [doubletake-bin](https://aur.archlinux.org/packages/doubletake-bin); they are upstream Linux packages, not this Windows release.

### Build, install and test Linux tools

```sh
make
make test
sudo make install
# Or a per-user custom prefix:
make install PREFIX="$HOME/.local"
# Remove a default-prefix installation:
sudo make uninstall
```

`make` builds `bin/doubletake`, `bin/doubletake-ctl` and `bin/doubletake-test-receiver`. Install copies binaries and man pages under `/usr/local` by default. Use the same `PREFIX`/`MANDIR` overrides for install and uninstall.

### Linux CLI examples

All non-loopback example addresses in these docs are reserved documentation addresses; replace them with your receiver's actual LAN address.

```sh
bin/doubletake                         # mDNS discovery and screen sharing
bin/doubletake -target 192.0.2.10 -pair # Pair with the prompted PIN
bin/doubletake -target 192.0.2.10 -no-audio
bin/doubletake -target 192.0.2.10 -fps 30 -bitrate 4500
bin/doubletake -target 192.0.2.10 -video-codec h264
bin/doubletake -target 192.0.2.10 -hwaccel nvenc
bin/doubletake -target 192.0.2.10 -hwaccel vaapi
bin/doubletake -target 192.0.2.10 -hwaccel openh264
bin/doubletake -target 192.0.2.10 -target-latency-ms 100
bin/doubletake -target 192.0.2.10 -debug
bin/doubletake -h
```

The standalone CLI defaults to the file credential backend at `~/.config/doubletake/credentials.json` (or `$XDG_CONFIG_HOME/doubletake/credentials.json`), unlike the Windows tray's explicit keyring backend. Select `-cred-backend keyring` for the supported OS keyring. Do not publish credential files. `-creds` overrides the file path. Prefer interactive credential entry; if using `DOUBLETAKE_CODE`, treat it as a secret and avoid shell history/log leakage. The variable takes precedence over `-code`; the command-line flag exposes secrets to process listings and history.

The upstream CLI supports capability-driven H.264/HEVC selection. Explicit `-video-codec hevc` can use NVENC or x265; automatic HEVC requires suitable negotiated receiver capabilities and a successful timestamped Main10 hardware preflight. SDR capture is not turned into HDR merely by selecting Main10. These upstream options do not imply Windows 4K/HDR verification; the Windows tray requests H.264 Full HD/60 fps at 10 Mbps. Explicit hardware selection fails when unavailable; automatic selection can try available encoders. On Windows, `-nvenc-device INDEX` pins a verified GStreamer NVIDIA H.264 device; the tray reads the optional per-user `DOUBLETAKE_NVENC_DEVICE` environment variable.

### Linux daemon and Plasma

```sh
bin/doubletake -daemonize -port-range 60000-60010
bin/doubletake-ctl status
bin/doubletake-ctl discover
bin/doubletake-ctl devices
bin/doubletake-ctl connect 192.0.2.10
bin/doubletake-ctl disconnect 192.0.2.10
bin/doubletake-ctl disconnect
bin/doubletake-ctl mute
bin/doubletake-ctl unmute
```

The Unix-socket daemon is separate from the Windows tray session protocol. `disconnect` without a target stops all streams. `mute`/`unmute` accept an optional target. `doubletake-ctl pin <PIN-or-password>` works with exactly one waiting receiver; with concurrent prompts, `connect TARGET PIN-or-password` identifies the receiver, but puts the supplied secret on the command line. The Plasma applet offers an inline credential prompt instead. Streams with the same negotiated codec/canvas can share a capture/encoder; different groups use independent encoders and bounded target queues.

### Optional AAC-ELD

The upstream sender chooses screen-audio formats from the receiver's advertised mask, preferring the built-in ALAC encoder. AAC-ELD is explicitly optional and needs `libfdk-aac` development headers/pkg-config metadata and cgo:

```sh
CGO_ENABLED=1 go build -tags fdk_aac -o bin/doubletake ./cmd/doubletake
```

Default Windows packaging does not enable this optional dependency. If a receiver only advertises AAC-ELD, a default build reports that it is unavailable instead of claiming an unsupported format. `-no-audio` allows CLI video/protocol diagnosis, not audio verification. Review the separate FDK AAC license/patent requirements before distributing such a build.

### Hardware-free protocol receiver

```sh
go build -o bin/doubletake-test-receiver ./cmd/doubletake-test-receiver
bin/doubletake-test-receiver -profile modern -listen 127.0.0.1:7000
# In a second terminal:
bin/doubletake -target 127.0.0.1 -port 7000 -test
```

On Windows, build with an `.exe` output name. The receiver profiles `modern`, `roku`, `lg`, `appletv3`, `uxplay` and `airserver` (`airtame` alias) exercise different advertised capabilities, pairing, SETUP ordering, timing, FairPlay and audio layouts. Authentication modes include `none`, `pin`, `password`, `digest` and `combined`; use the receiver's help/man page for options. `-listen 127.0.0.1:0` chooses an ephemeral port; `-stats-interval 1s` prints counters.

This sink checks control and media traffic and validates legacy decrypted video framing in selected profiles. It does not decode/display video or play audio, and profile names are not evidence of Windows hardware support. Sender behavior follows capabilities and the protocol actually negotiated, not receiver names or model strings.
