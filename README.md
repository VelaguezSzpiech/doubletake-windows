# DoubleTake for Windows

An **unofficial Windows port** of [omarroth/doubletake](https://github.com/omarroth/doubletake), the open-source AirPlay screen-mirroring sender. Share your Windows desktop and system audio with an Apple TV using its native AirPlay receiver. **Nothing needs to be installed on the TV.** This project is not affiliated with or endorsed by Apple or the upstream maintainer.

Windows modifications are maintained in [VelaguezSzpiech/doubletake-windows](https://github.com/VelaguezSzpiech/doubletake-windows). The upstream Go protocol implementation, Linux sources, man pages and Plasma widget remain available; see the [Linux reference](docs/building.md#upstream-linux-reference).

## Install and connect

1. Download `DoubleTake-Windows-1.0.0-Setup.exe` from the [latest release](https://github.com/VelaguezSzpiech/doubletake-windows/releases/latest). Releases also provide `SHA256SUMS.txt` for checking the downloaded installer.
2. Run the installer. It installs for your current Windows user, normally under `%LOCALAPPDATA%\Programs\DoubleTake`, with a Start menu shortcut and an optional Desktop shortcut. It does **not** enable login autostart.
3. Setup is **network-assisted**: it requires internet access only if a complete compatible GStreamer runtime is missing. In that case it downloads the official 64-bit MSVC GStreamer 1.28.6 installer, checks its pinned SHA-256 before running it, and checks the installed runtime. A download, hash or runtime failure stops setup visibly. **.NET is bundled self-contained**; end users do not need Go, a .NET SDK or a separate .NET runtime.
4. On the Apple TV, enable AirPlay in **Settings → AirPlay and HomeKit**. Put the PC and TV on a network that permits local device-to-device traffic.
5. Launch **DoubleTake** from the Start menu. Single-click its taskbar notification-area icon (check the hidden-icons menu if necessary) to open the flyout.
6. Select your receiver using the **settings gear**, then click **Connect**. If discovery is empty, enter the TV's actual LAN IP and AirPlay port there. Manual IP can bypass multicast discovery blocked by a VPN; it cannot bypass a VPN route or policy that blocks the LAN.
7. If requested, enter the TV's PIN inline and continue. A receiver configured with a fixed password asks for that password instead. Saved pairing keys are kept in **Windows Credential Manager**, not a public source/config file.

The installer is **unsigned**. Windows may display SmartScreen or an unknown-publisher warning. Check the release source and checksum and follow your organization's policy; this project does not recommend disabling Windows protections.

Click outside the flyout or press **Escape** to hide it without stopping sharing. Credential entry stays open until continued or canceled. Click the icon to reopen/toggle the flyout; launching the shortcut again opens the existing instance. **Disconnect** stops sharing; right-click the icon and choose **Quit DoubleTake** to exit. The gear includes **Open log** for local diagnostics.

## Requirements and current limits

- **Windows 10 or newer, x64**, with a graphics driver supporting D3D11 Desktop Duplication. Use a currently supported Windows edition for security updates.
- A compatible AirPlay receiver and a reachable local network. The TV uses native AirPlay, not a companion app.
- The **64-bit MSVC GStreamer runtime**, including desktop capture, WASAPI loopback, an H.264 encoder and the required parser/RTP plugins. Setup detects normal user/machine installations or installs the pinned runtime when needed.
- **Primary monitor only**: this is desktop mirroring, not a multi-monitor picker, per-window capture or an extended display.
- Audio captures the **default Windows playback device** using WASAPI loopback. It does not create a separate Windows audio output device, capture the microphone, or pass through Dolby Atmos/DTS bitstreams. Select the desired playback device before connecting; an output-device change may require disconnecting and reconnecting.
- The tray requests **Full HD H.264 at 60 fps and 10 Mbps**. Explicit receiver size ceilings are respected; when dimensions are omitted, 1080p is a sender request rather than an advertised capability. Windows prefers native D3D11 NVENC with GPU conversion/scaling. Set `DOUBLETAKE_NVENC_DEVICE` to an explicitly verified GStreamer NVIDIA device index to pin an encoder; explicit selection fails rather than silently switching GPU. This is not a promise of universal receiver compatibility, 4K or HDR.
- Protected content, display/driver changes and network conditions can affect capture or playback. Reconnect after an output change if sharing stops or audio remains on the previous device.

## Verification and compatibility

Windows source verification includes the full Go suite across all packages and a .NET build with zero warnings or errors. New tests cover explicit GPU/canvas selection and bounded retries that preserve the exact encrypted packet. This does not claim a fresh Linux execution.

On AppleTV11,1 (Apple TV 4K, second generation), a later session negotiated H.264 1920×1080 at 60 fps using a GTX 1650; after reconnect, the user confirmed visible video and audible audio. This is one setup, not a compatibility guarantee or long-duration stability result. Long-run audio degradation followed by `WSAENOBUFS` and a TCP abort remains unresolved; short healthy recovery is not a long soak. Capture-probe startup can stall. The 75/85 ms scheduled timing values are not measured end-to-end latency; a joint 40 ms test was reverted after audio corruption.

The public Windows v1.0.0 installer is unchanged and does not include these current-source features. To use them, build from source; see the [Unreleased changelog](CHANGELOG.md).

## Documentation

- [Building Windows, testing, and upstream Linux reference](docs/building.md)
- [Architecture, data flow, privacy and security](docs/architecture.md)
- [Logs and troubleshooting: installation, VPN, pairing, audio and capture](docs/troubleshooting.md)
- [Unreleased changes](CHANGELOG.md)
- [Third-party notices and source links](THIRD_PARTY_NOTICES.md)
- [Installer implementation notes](packaging/windows/README.md)


## Privacy and security

DoubleTake has no automatic external telemetry or log-upload feature in its own application code. Discovery sends mDNS traffic on the local network, and mirroring sends your captured desktop and playback audio to the selected receiver. The installer contacts the official GStreamer download host if the runtime is missing. OS services, the receiver and third-party dependencies have their own network/privacy behavior; this is not a blanket guarantee that every component makes no external connection.

Receiver preferences and diagnostic logs are local under `%LOCALAPPDATA%\DoubleTake\state`; pairing secrets use Windows Credential Manager. Logs may still identify devices, addresses and local paths: review and redact them before sharing. Upgrade/uninstall preserves these preferences, logs and saved pairing; uninstall also leaves the separately installed GStreamer runtime. See [security boundaries](docs/architecture.md#privacy-and-security-boundaries).

## Attribution, license and disclaimer

The AirPlay sender originates from **[omarroth/doubletake](https://github.com/omarroth/doubletake)**. This fork adds the native Windows capture/process integration, WPF taskbar flyout, Windows packaging and documentation; it does not claim authorship of the upstream protocol work. The upstream author credits earlier reverse-engineering projects, including [UxPlay](https://github.com/FDH2/UxPlay), and notes that much of the code was written with LLM assistance. The Windows port is not a substitute for a production/security audit; do not rely on it in security-sensitive environments.

The project remains **GNU Lesser General Public License v3.0 or later** (`LGPL-3.0-or-later`): see [LICENSE](LICENSE) and its incorporated GPLv3 terms in [COPYING.GPL](COPYING.GPL). Upstream releases v0.3.2 and earlier used `GPL-3.0-or-later`. Full source is provided in this repository; dependency licenses are separate and listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). Apple, AirPlay and Apple TV are trademarks of Apple Inc.
