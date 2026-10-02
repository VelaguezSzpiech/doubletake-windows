# Architecture

DoubleTake for Windows is an unofficial native Windows front end and capture port around the upstream [doubletake](https://github.com/omarroth/doubletake) Go AirPlay sender. The full upstream source remains in this repository; the Windows installer distributes built components, not a replacement closed-source backend.

## Components

| Component | Responsibility |
| --- | --- |
| `windows-tray/` | .NET 8 WPF flyout, WinForms notification-area icon, receiver preferences, inline credential entry, local logs and backend supervision |
| `cmd/doubletake/` | Go sender entry point; interactive CLI, JSON discovery and JSON UI session control |
| `internal/airplay/` | mDNS discovery, capability negotiation, RTSP/HTTP control, pairing/FairPlay, clock synchronization, encrypted video/audio transport and GStreamer capture integration |
| GStreamer (external runtime) | D3D11 Desktop Duplication, WASAPI playback loopback, pixel conversion/scaling, H.264 encoding and parser/RTP framing stages |
| Windows Credential Manager | Persistent pairing identity through `go-keyring`/`wincred` |
| `scripts/build-windows.ps1`, `packaging/windows/` | Self-contained win-x64 publish, Go build, dependency notices and per-user Inno Setup installer |
| Retained Linux tools | Wayland/PipeWire portal and X11 capture, Unix-socket daemon, `doubletake-ctl`, Plasma widget and diagnostic receiver |

The installer keeps `DoubleTake.Tray.exe`, `doubletake.exe` and the self-contained .NET runtime together. GStreamer is separately installed/reused; it is not committed or bundled into the app directory. No Go/.NET SDK is needed to run the installed app.

## Session and media flow

```text
Notification-area icon / WPF flyout
  ├─ discovery request → Go -discover-json → local mDNS → receiver list
  ├─ selected receiver + settings → Go -ui (supervised child)
  ├─ inline PIN/password → stdin JSON credential reply
  └─ status / warnings ← stdout JSON events

Primary monitor → GStreamer D3D11 capture → conversion/scaling → H.264 encoder
  → timestamped media framing → Go AirPlay video transport → Apple TV

Default playback endpoint → GStreamer WASAPI loopback → PCM
  → Go negotiated screen-audio encoder/transport → Apple TV

Backend stderr → bounded local diagnostic log
Pairing identity → Windows Credential Manager
```

The tray invokes discovery independently, then starts a selected receiver session with its discovery advertisement (when available), explicit address/port, `-cred-backend keyring`, `-video-codec h264` and `-fps 30`. It removes inherited `DOUBLETAKE_CODE` from the backend environment so the UI owns credential entry. It does not place PINs/passwords on the command line.

The UI protocol is newline-delimited JSON. The backend waits for `start` before it can begin capture, allowing the tray to assign its kill-on-close Windows Job Object first. Credential replies include the corresponding `request_id`. `stop` or stdin EOF cancels the session. The backend reports connected readiness after initial video **and** audio sends; this cannot attest that the remote device decoded/displayed frames or played sound.

Windows Job Objects contain the session backend and capture subprocesses. The capture launcher starts GStreamer suspended, assigns its process job, then resumes it. Disconnect/Quit tears down the session rather than leaving an unattended capture worker. A single-instance tray launch reopens the existing flyout rather than starting a second tray.

The flyout is dismissed by Escape/outside interaction without ending an active session; pending credential entry stays visible until continued or canceled. These controls are not a capture-permission boundary: sharing continues until Disconnect/Quit or the session ends.

## Capture, encoding and negotiation

Windows video uses `d3d11screencapturesrc` with `monitor-index=-1`, selecting the **primary monitor**, followed by `d3d11download` and the shared timestamp-aware encoding pipeline. It is not an all-monitor, window-selection or virtual-display implementation. The existing encoder policy may select NVENC when usable or a software encoder when available. Driver/desktop conditions and protected content can limit capture.

The tray requests H.264/30 fps, but the receiver's post-SETUP display information determines the canvas. A nominal canvas is distinct from an advertised decoder maximum. The observed Windows session was 1280×720 at 30 fps with NVENC; neither a larger decoder ceiling nor retained upstream HEVC code constitutes Windows 4K/HDR verification.

Audio uses `wasapi2src loopback=true` on the default render endpoint, not a microphone or a new output device. Go encodes the captured PCM in the receiver-negotiated screen-audio format (normally the built-in ALAC path). This is not Dolby Atmos/DTS passthrough. Optional AAC-ELD requires a separately enabled FDK AAC build and is not part of default Windows packaging. A playback-device change may require reconnecting to reopen the correct endpoint.

Receiver capabilities from discovery and `/info`, accepted SETUP shapes, and the pairing/encryption protocol actually negotiated determine wire behavior. The sender supports upstream SRP-6a PIN/password flows, CoreUtils/HAP and legacy variants, FairPlay SAP and negotiated encrypted media/control channels. ChaCha20-Poly1305 and legacy AES-based paths apply according to the negotiated protocol; it is not accurate to promise one cipher or modern encrypted control for every receiver.

Receiver display metadata is refreshed after control SETUP (or the accepted audio SETUP on a negotiated media-first path) when needed. The sender begins with a control-only SETUP and uses a bounded media-first alternate on explicit rejection; audio descriptor negotiation is a separate axis. NTP/PTP timing uses the shared presentation policy. Upstream automatic defaults are 75 ms video and 85 ms screen audio; the high-resolution path can add a shared measured scheduling margin. These scheduling values are not a guaranteed end-to-end visible latency.

## Local state and packaging

| Location | Purpose |
| --- | --- |
| `%LOCALAPPDATA%\Programs\DoubleTake` (default) | Installed application/runtime and license files |
| `%LOCALAPPDATA%\DoubleTake\state\receiver.json` | Selected receiver name/IP/port preferences |
| `%LOCALAPPDATA%\DoubleTake\state\doubletake.log` | Local diagnostics; rotates to `.previous` after the size threshold |
| Windows Credential Manager, service `doubletake` | Saved per-receiver pairing identity |

The log is bounded by rotation at approximately 4 MiB, with one previous file. Application state is outside source and installation directories. The tray selects the keyring backend; a `credentials.json` path passed to the backend does **not** imply that the tray stores pairing keys there. Standalone CLI defaults differ: its default backend is a JSON credential file unless explicitly changed.

Setup defaults to per-user installation, provides an uninstall entry and Start menu shortcut, and optionally a Desktop shortcut. Login autostart is not enabled. If a complete GStreamer runtime is missing, setup downloads the pinned official installer over HTTPS, checks SHA-256 before executing, and checks the resulting runtime. .NET comes from self-contained publishing. Upgrade/uninstall preserves user state and Windows Credential Manager pairing and leaves the external GStreamer runtime.

## Privacy and security boundaries

- **Captured content is shared content.** The chosen TV receives your primary desktop and default playback audio. Hide sensitive windows and notifications before connecting. This is not an access-control or DRM solution.
- **Local discovery is network traffic.** mDNS exposes discovery information to the local network; manually supplying an address avoids dependence on multicast but still needs a permitted route to the receiver.
- **Credentials are local secrets.** The tray sends entered credentials through redirected stdin JSON, not arguments or diagnostics, and persists pairing in Windows Credential Manager. That is OS-managed storage, not immunity to a compromised user account or machine. Standalone CLI file-backend credentials must also be protected.
- **Logs are not anonymous.** Debug diagnostics omit shared secrets, derived encryption keys, raw protocol bodies and captured-media bytes, retaining status, algorithms and sizes. Diagnostics can still contain receiver names, addresses and local paths. Review/redact before posting; never publish credential exports or raw unreviewed logs.
- **No automatic external telemetry in the application.** DoubleTake's own code has no automatic analytics/log-upload feature. The installer may contact the official GStreamer host; source builds contact dependency feeds. Windows/.NET services, downloaded dependencies and the receiver may have independent network behavior. No claim is made that all dependencies are telemetry-free or that all traffic stays offline.
- **Hashes are integrity checks, not publisher signatures.** The Windows installer is unsigned and may prompt SmartScreen/unknown-publisher warnings. Obtain it from the intended release and evaluate it under your own security policy; do not disable protections to satisfy setup.
- **Reverse-engineered compatibility is limited evidence.** Upstream reports and synthetic protocol fixtures do not establish every receiver, codec or Windows configuration. The observed Windows hardware check covers one AppleTV11,1 setup, with visible desktop output and transport-level audio sends, not independently confirmed speaker audibility.

This is community software with substantial upstream LLM-assisted code and no independent security certification. Do not treat it as a production/security-sensitive mirroring solution.

## Retained upstream Linux design

Linux uses PipeWire/xdg-desktop-portal on Wayland and X11 capture on Xorg, with PulseAudio monitor capture for system audio. Wayland preflight retains the user-authorized portal source before starting an encoder. The separate daemon uses a Unix socket and groups streams by negotiated codec/even canvas; shared groups fan out through bounded per-target queues so a stalled target can detach without blocking peers. The Plasma widget talks to that daemon. These sources remain useful reference and are not invoked by the Windows tray.

See [building and Linux usage](building.md#upstream-linux-reference), [troubleshooting](troubleshooting.md), the retained man pages and [third-party notices](../THIRD_PARTY_NOTICES.md).
