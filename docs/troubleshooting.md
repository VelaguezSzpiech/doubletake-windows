# Troubleshooting

These instructions cover the unofficial Windows port. [Upstream Linux notes](#upstream-linux-troubleshooting) are retained below. Replace any example address with your receiver's actual LAN address; `192.0.2.10` is a reserved documentation address, not a usable TV address.

## Find the local log

Open the flyout's **settings gear → Open log**, or right-click the taskbar notification-area icon and select **Open local log**. The current file is:

```text
%LOCALAPPDATA%\DoubleTake\state\doubletake.log
```

Rotation retains `doubletake.log.previous`. Receiver preferences are adjacent in `receiver.json`; pairing keys are in **Windows Credential Manager**, not in receiver preferences. Upgrade/uninstall preserves this state, so uninstalling is not a pairing reset.

Logs can contain device names, IP addresses, local paths and protocol details even though credential input is not intentionally logged. Before reporting an issue, redact those details and include only the relevant error excerpt. Never attach Credential Manager exports, pairing files, PINs/passwords, an entire unreviewed log or private network configuration. Useful non-sensitive information includes Windows version, public receiver model identifier, GStreamer version, encoder type, and steps that reproduce the error. Report Windows-specific issues to [this fork](https://github.com/VelaguezSzpiech/doubletake-windows/issues).

## Installation fails or Windows warns

- The installer is unsigned. A SmartScreen/unknown-publisher warning is possible. Confirm that the download is from the intended [release](https://github.com/VelaguezSzpiech/doubletake-windows/releases/latest) and compare its SHA-256 with the release's `SHA256SUMS.txt`. Follow your organization's policy; do not disable protections.
- Setup only needs internet for a missing/incomplete GStreamer prerequisite. Proxy, captive portal, offline access or a blocked official download host can prevent that step. Complete legitimate network access or install the official complete 64-bit MSVC runtime first, then retry setup.
- A **hash mismatch** is a stop condition. Do not run that prerequisite or bypass verification. Obtain a fresh official download and report a persistent mismatch without sharing sensitive network diagnostics.
- .NET is bundled self-contained. End users should not need an SDK or separate .NET runtime. If running a source-published payload, keep the entire app directory and adjacent `doubletake.exe` together; copying only the tray executable is insufficient.
- Quit DoubleTake before an upgrade if files remain in use. A separately installed GStreamer runtime and saved state/pairing remain after uninstall; that is intentional.

## TV missing, VPN or connection timeout

1. Enable AirPlay on the TV and check its current address in the TV's network settings. Verify the PC can reach the same LAN; guest Wi-Fi and client isolation often prevent peer traffic even when both devices have internet access.
2. Refresh receivers in the gear/settings view. Discovery uses multicast mDNS; VPN adapters, separate VLANs and multicast filtering can hide a reachable receiver.
3. Enter the **actual TV LAN IP** and AirPlay port manually in the gear. The usual port is `7000`; use the advertised/configured port if different. Manual IP avoids the discovery requirement, not the network connection requirement.
4. If a VPN is in use, check its allowed local-LAN or split-routing options under your organization's policy. Full-tunnel or policy-blocked LAN routes can still prevent manual connections. You do not need to disable the VPN merely to use manual addressing, but the permitted route must exist.
5. Allow the installed backend through Windows Firewall only on the appropriate trusted/private network if required. Do not turn off the firewall or broadly expose AirPlay traffic to public networks. RTSP/video/event channels use outbound TCP connections; timing/audio also use UDP, and some receiver timing modes need inbound UDP.

A public IP, documentation example address or the PC's own address is not the TV address. Recheck a saved manual address after a DHCP change. An IP entry alone is not receiver authentication; use the pairing prompt for the intended device.

## PIN, password or saved-pairing problems

- Enter the PIN displayed by the TV into the inline prompt. Keep the flyout open until you continue or cancel; credential entry intentionally does not auto-hide on outside interaction.
- **Require Password** means the receiver's configured fixed password, not a rotating onscreen PIN. A password prompt may appear without a code on the TV. Pairing and HTTP Digest authentication are distinct protocol steps even when they share one user-facing credential flow.
- An HTTP `401` can mean the receiver is asking for its configured password. Do not post that password in an issue or put it on a command line.
- Saved credentials may become invalid if the receiver's pairing/security settings change. Use the settings option to pair again, then reconnect. The Windows tray saves pairing in Credential Manager under the `doubletake` service; uninstall/reinstall does not clear it.
- If pairing works but cannot be saved, inspect the local warning and check access to your Windows user's Credential Manager. Avoid exporting entries or replacing protected storage with a public config file.

## Connected but no visible picture

The connected state means the backend sent initial video and audio media; it is not a remote playback acknowledgment. Check the TV is showing the AirPlay session, that the expected **primary monitor** is being shared, and whether the log reports later capture or receiver errors.

Only the primary monitor is captured; a window on another monitor will not appear. Protected/DRM content may be blank. Locking, remote desktop, monitor/GPU changes or a driver reset can affect Desktop Duplication. Return to an interactive desktop and disconnect/reconnect after changing display output. Check the graphics driver and required GStreamer capture elements if this persists.

The initial Windows check was H.264 720p30. A later AppleTV11,1 session used H.264 1920×1080 at 60 fps with a GTX 1650; after reconnect, visible video and audible audio were user-confirmed. This is not a long-duration stability result. Long-run audio degradation followed by `WSAENOBUFS` and a TCP abort remains unresolved; short healthy recovery is not a long soak, and capture-probe startup can stall.

The tray requests Full HD H.264/60 fps at 10 Mbps, respecting explicit smaller receiver ceilings. Optional explicit GPU selection is available; a persisted per-user setting takes precedence over an inherited environment value. For example, set the per-user environment variable `DOUBLETAKE_NVENC_DEVICE` to the verified device index (replace `INDEX`): `$env:DOUBLETAKE_NVENC_DEVICE = "INDEX"` is only a process-scoped PowerShell example, not persisted. Persist it in Windows user environment settings with value `INDEX`, then restart DoubleTake. An explicit GPU choice fails rather than silently falling back. These source features are not in the unchanged public v1.0.0 installer; build from source (see [Unreleased changelog](../CHANGELOG.md)).

The 75/85 ms scheduled timing values are not measured end-to-end latency. A joint 40 ms test was reverted after audio corruption. Higher picture quality and GPU encoding do not guarantee gaming-grade latency.

## No sound, wrong sound or output changed

1. Before connecting, choose the intended **default Windows playback device** and play an ordinary audible source. Check Windows app/output volume and the TV/speaker volume.
2. If an app explicitly routes audio to a different device, its audio may not be in the default endpoint's loopback stream. Route that app to the default device if you want it included.
3. Disconnect and reconnect after changing default playback device, plugging/unplugging an audio output, or resuming from a display/output change. The capture endpoint may need reopening.
4. Look for `wasapi2src` or audio-format/capture errors in the local log. The tray requires both video and audio startup; missing audio support is not reported as a successful full-sharing session.

DoubleTake captures playback PCM through WASAPI loopback. It does **not** create a separate Windows output device, capture your microphone, or provide Dolby Atmos/DTS passthrough. The physical Windows verification showed audio transport sends, but speaker audibility was not separately confirmed. A receiver that only advertises unsupported AAC-ELD audio needs a separately enabled optional build; the default Windows release does not bundle FDK AAC.

On Windows, UDP error `WSAENOBUFS` (10055, “insufficient buffer space” or “queue was full”) triggers at most three retries of the exact same encrypted datagram, at 2 ms intervals. Persistent buffer pressure or any permanent error still fails the send/session; retries do not guarantee delivery or resolve long-run audio degradation. A later session recovered briefly, but long-run degradation followed by `WSAENOBUFS` and TCP abort remains unresolved; this was not a long soak. Non-Windows behavior is unchanged. Check local logs and Windows resource/network events if it repeats.

## GStreamer or encoder errors

Use the **MSVC x86_64** runtime, not a 32-bit or MinGW package. A partial installation can contain `gst-launch-1.0.exe` but still lack capture/audio/encoding plugins. The installer checks required files; runtime element checks remain useful when drivers, plugin loading or custom installations differ.

If GStreamer is on your terminal PATH, these commands inspect the relevant elements without starting desktop sharing:

```powershell
gst-inspect-1.0 --version
gst-inspect-1.0 d3d11screencapturesrc
gst-inspect-1.0 d3d11download
gst-inspect-1.0 wasapi2src
gst-inspect-1.0 h264parse
gst-inspect-1.0 rtph264pay
gst-inspect-1.0 x264enc
gst-inspect-1.0 nvh264enc
```

`nvh264enc` is optional and depends on supported NVIDIA hardware/drivers; an absent NVENC element does not alone prove software capture is broken. At least one usable H.264 encoder is needed. Repair a partial official runtime rather than downloading individual DLLs from unrelated sites. A custom runtime root can be supplied through `GSTREAMER_1_0_ROOT_MSVC_X86_64`; it must contain `bin` and the matching plugins.

The tray discovers normal user/machine GStreamer paths and configures PATH for its children. That does not mean `gst-inspect-1.0` will be found by every already-open terminal; invoke it from the runtime's `bin` directory or use its full executable path. Keep custom DLL/plugin versions consistent.

An incomplete **higher-priority** runtime can shadow a newly installed complete one. Search order is the environment override, the normal per-user runtime, the Program Files runtime, then the conventional `C:\gstreamer\1.0\msvc_x86_64` location. Repair the incomplete runtime or remove an obsolete environment override through normal Windows settings, then restart DoubleTake/setup. Setup fails visibly if the shadow remains; it does not silently ignore the runtime the application would select.

## Flyout or sharing seems stuck

Single-click the notification-area icon to toggle the flyout; Escape or outside click hides it, not the session. If Windows placed the icon in its hidden-icons menu, open it there. Launching the shortcut again opens the existing instance. A pending PIN/password prompt stays visible intentionally.

Use **Disconnect** to stop sharing or **Quit DoubleTake** from the icon menu to exit. If a session errors, open the log rather than assuming that an earlier connected state proves ongoing playback. Display/audio-output changes may need a fresh session.

## Upstream Linux troubleshooting

These are retained CLI/daemon notes, not controls in the Windows flyout.

### Firewall and timing

The sender reserves three consecutive UDP ports for timing and audio; default allocation uses OS ephemeral ports. Receiver-initiated NTP can probe an inbound timing port during SETUP; PTP/sender-initiated NTP do not require that inbound timing probe. Event/video TCP connections are outbound to receiver-provided ports.

Use a bounded range and a narrow rule for the actual receiver if needed:

```sh
bin/doubletake -target 192.0.2.10 -port-range 60000-60010
sudo ufw allow from 192.0.2.10 proto udp to any port 60000:60010
```

Replace the example address; use equivalent scoped nftables/firewalld rules when appropriate. Daemon mode needs at least three free ports per simultaneous receiver in the shared range. `-target-latency-ms` overrides joint video/audio scheduling; it does not fix a blocked route or missing capture plugin.

### Linux capture/audio

On Wayland, check the PipeWire/xdg-desktop-portal screen-sharing permission and source selection. On X11, check `DISPLAY` and the primary monitor geometry; the standalone CLI also has X11 window-selection options. Windows deliberately rejects those X11-specific selections. For system audio, ensure the PulseAudio monitor source and `pactl` are available.

`-hwaccel auto` can try available encoders; explicitly selected encoders fail rather than silently changing the selection. `-no-audio` is useful for isolating video/protocol issues in the CLI but does not establish audio support. A nonzero advertised audio mask with neither supported ALAC nor enabled AAC-ELD is rejected.

See the [Linux build/CLI reference](building.md#upstream-linux-reference), retained [man pages](../man/man1/) and [Plasma documentation](../plasmoid/README.md). Debug output and standalone file-backend pairing credentials still require careful handling before sharing.
