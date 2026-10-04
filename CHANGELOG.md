# Changelog

## Unreleased — 2026-10-04

- Request Full HD H.264 video at 60 fps and 10 Mbps while respecting explicit receiver size ceilings; the Windows tray requests `-full-hd`.
- Use GPU-side D3D11 preprocessing for NVENC. `-nvenc-device` supports explicit encoder selection; persisted per-user `DOUBLETAKE_NVENC_DEVICE` takes precedence over inherited environment. Explicit GPU selection fails rather than silently falling back.
- On Windows, retry `WSAENOBUFS` UDP sends up to three times at 2 ms intervals, resending the exact encrypted datagram. Persistent buffer pressure and permanent errors still fail; non-Windows behavior is unchanged.
- Tests cover explicit GPU/canvas selection and bounded identical encrypted-packet retries. Full Windows Go tests passed across all packages; the .NET build completed with zero warnings or errors. No fresh Linux execution is claimed.
- On AppleTV11,1, a later H.264 1080p60 GTX 1650 session showed visible video and audible audio after reconnect, as confirmed by the user. This is not a long soak: audio degradation followed by `WSAENOBUFS`/TCP abort remains unresolved, and capture-probe startup can stall. The 75/85 ms scheduled timing values are not end-to-end measurements; a joint 40 ms test was reverted after audio corruption.
- The public Windows v1.0.0 installer is unchanged and does not include these current-source features; build from source to use them.
