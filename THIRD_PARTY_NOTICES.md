# Third-party notices

DoubleTake for Windows is an unofficial Windows port of [omarroth/doubletake](https://github.com/omarroth/doubletake). The upstream AirPlay sender and Linux tooling remain upstream work; this fork adds Windows capture/process integration, a WPF taskbar flyout, packaging and documentation. It is not affiliated with or endorsed by Apple or the upstream maintainer.

The project is licensed under **GNU Lesser General Public License v3.0 or later** (`LGPL-3.0-or-later`). Keep [LICENSE](LICENSE) and [COPYING.GPL](COPYING.GPL) with redistributions. Upstream versions v0.3.2 and earlier were `GPL-3.0-or-later`. Corresponding project source is available at <https://github.com/VelaguezSzpiech/doubletake-windows>, with upstream source at <https://github.com/omarroth/doubletake>. The Windows release tag is `windows-v1.0.0` for installer version `1.0.0`.

The upstream author acknowledges earlier AirPlay reverse-engineering projects, including [UxPlay](https://github.com/FDH2/UxPlay), and notes substantial LLM-assisted implementation. That acknowledgment is not a claim that this fork owns or independently reimplemented all underlying protocol research. Apple, AirPlay and Apple TV are trademarks of Apple Inc.

This file identifies dependencies and their source/license locations. Their own licenses govern those components; the project's LGPL does not relicense them. Windows builds collect actual dependency license files alongside the application under `licenses/go` and `licenses/dotnet`, in addition to this notice and the project's license texts. Preserve all supplied notices when redistributing and check the resolved versions for a modified build.

## Go runtime and modules

The Go backend statically incorporates the Go runtime/standard library and the relevant modules selected by platform/build tags. The source manifest is [`go.mod`](go.mod), with checksums in [`go.sum`](go.sum). Listing a module here does not imply every module is linked into every Windows binary; some serve retained Linux code or tooling.

[Go](https://go.dev/) is provided under a BSD-style license, copyright The Go Authors; see [Go LICENSE](https://github.com/golang/go/blob/master/LICENSE) and its [source](https://github.com/golang/go). The build includes the installed Go toolchain's LICENSE. The manifest requires Go 1.25.0 or newer; a particular builder may use a newer toolchain.

| Module | Manifest version | License / source |
| --- | --- | --- |
| `github.com/aead/chacha20poly1305` | `v0.0.0-20201124145622-1a5aba2a8b29` | MIT; [source and license](https://github.com/aead/chacha20poly1305/tree/1a5aba2a8b29) |
| `github.com/aead/chacha20` | `v0.0.0-20180709150244-8b13a72661da` | MIT; [source and license](https://github.com/aead/chacha20/tree/8b13a72661da) |
| `github.com/aead/poly1305` | `v0.0.0-20180717145839-3fee0db0b635` | MIT; [source and license](https://github.com/aead/poly1305/tree/3fee0db0b635) |
| `github.com/godbus/dbus/v5` | `v5.2.2` | BSD-2-Clause; [source and license](https://github.com/godbus/dbus/tree/v5.2.2) |
| `github.com/grandcat/zeroconf` | `v1.0.0` | MIT; [source and license](https://github.com/grandcat/zeroconf/tree/v1.0.0) |
| `github.com/zalando/go-keyring` | `v0.2.8` | MIT; [source and license](https://github.com/zalando/go-keyring/tree/v0.2.8) |
| `github.com/danieljoos/wincred` | `v1.2.3` | MIT; [source and license](https://github.com/danieljoos/wincred/tree/v1.2.3) |
| `github.com/cenkalti/backoff` | `v2.2.1+incompatible` | MIT; [source and license](https://github.com/cenkalti/backoff/tree/v2.2.1) |
| `github.com/miekg/dns` | `v1.1.62` | BSD-3-Clause; [source and license](https://github.com/miekg/dns/tree/v1.1.62) |
| `golang.org/x/crypto` | `v0.31.0` | BSD-style; [source and notices](https://github.com/golang/crypto/tree/v0.31.0) |
| `golang.org/x/mod` | `v0.22.0` | BSD-style; [source and notices](https://github.com/golang/mod/tree/v0.22.0) |
| `golang.org/x/net` | `v0.33.0` | BSD-style; [source and notices](https://github.com/golang/net/tree/v0.33.0) |
| `golang.org/x/sync` | `v0.10.0` | BSD-style; [source and notices](https://github.com/golang/sync/tree/v0.10.0) |
| `golang.org/x/sys` | `v0.28.0` | BSD-style; [source and notices](https://github.com/golang/sys/tree/v0.28.0) |
| `golang.org/x/tools` | `v0.28.0` | BSD-style; [source and notices](https://github.com/golang/tools/tree/v0.28.0) |
| `howett.net/plist` | `v1.0.1` | BSD-2-Clause plus included Go BSD notice; [source and license](https://github.com/DHowett/go-plist/tree/v1.0.1) |

MIT copyright notices include Andreas Auernhammer (2016 for chacha20/poly1305; 2017 for chacha20poly1305), Zalando SE (2016), Daniel Joos (2014), Cenk Altı (2014), and Stefan Smarzly (2016)/Oleksandr Lobunets (2014) for zeroconf. Zeroconf's notice identifies portions originating in the bonjour project. `go-keyring` also contains an internal shellescape helper with an MIT notice copyright Alessio Treglia (2016).

BSD copyright notices include Georg Reinke and Google (2013) for godbus; The Go Authors (2009) and Miek Gieben (2011) for DNS; Dustin L. Howett (2013) and included Go Authors (2012) notices for plist; and The Go Authors for Go extension modules. Consult each component's supplied license for exact conditions and any additional per-file notices.

## .NET, WPF and Windows integration

The tray targets `net8.0-windows`, uses WPF/Windows Forms, and is published **self-contained for win-x64**. The .NET runtime and Windows Desktop runtime are included in the application payload; no .NET SDK is included or required for end users.

- [.NET runtime source](https://github.com/dotnet/runtime), [license](https://github.com/dotnet/runtime/blob/main/LICENSE.TXT) and [third-party notices](https://github.com/dotnet/runtime/blob/main/THIRD-PARTY-NOTICES.TXT).
- [WPF source](https://github.com/dotnet/wpf), [license](https://github.com/dotnet/wpf/blob/main/LICENSE.TXT) and [third-party notices](https://github.com/dotnet/wpf/blob/main/THIRD-PARTY-NOTICES.TXT).
- [Windows Forms source](https://github.com/dotnet/winforms), [license](https://github.com/dotnet/winforms/blob/main/LICENSE.TXT) and [third-party notices](https://github.com/dotnet/winforms/blob/main/THIRD-PARTY-NOTICES.TXT).

These projects use the MIT license with separate third-party notices. The resolved .NET 8 servicing version depends on the installed SDK/runtime packs. The build collects the actual referenced packs' license files into `licenses/dotnet`. Because the Windows Desktop pack omits component notices, the build obtains WPF and Windows Forms notices from the exact source commits identified by that pack's provenance, recording immutable source URLs alongside them. Retain those files, not merely links to the current development branch, when redistributing. Windows itself, its APIs and graphics/audio drivers are operating-system prerequisites governed separately.

## GStreamer and multimedia codecs

GStreamer is an **external prerequisite**, distributed by its official project, not committed to this repository or embedded in the DoubleTake app payload. The network-assisted installer reuses a complete existing runtime or downloads the pinned official **MSVC x86_64 1.28.6** user installer, checks its SHA-256, and installs it separately.

- [Project and downloads](https://gstreamer.freedesktop.org/)
- [Source repository](https://gitlab.freedesktop.org/gstreamer/gstreamer)
- [GStreamer licensing guidance](https://gstreamer.freedesktop.org/documentation/application-development/appendix/licensing.html)
- [Pinned Windows installer](https://gstreamer.freedesktop.org/data/pkg/windows/1.28.6/msvc/gstreamer-1.0-msvc-x86_64-1.28.6.exe)
- SHA-256: `059251444d1267b486eba390b18d25fed87e10315e72f757ec6c7e912fa746b5`

GStreamer core and many plugins are LGPL-licensed, but that is **not a blanket license statement for every plugin or linked codec**. The official distribution includes components and notices under different licenses. The Windows port uses D3D11 desktop capture, WASAPI2 loopback, H.264 encoder/parser and RTP elements; actual encoder availability depends on installed plugins and drivers. NVENC additionally depends on NVIDIA hardware/driver components. x264, x265, OpenH264, libav and other optional codecs can have their own copyright, license and patent considerations. Refer to the official installed distribution's license files and each selected plugin's dependencies before repackaging it.

The default Go build has a built-in ALAC audio path. Optional upstream AAC-ELD builds use [FDK AAC](https://github.com/mstorsjo/fdk-aac), with its [separate license](https://github.com/mstorsjo/fdk-aac/blob/master/NOTICE). FDK AAC is **not included/enabled by default Windows packaging**. Codec patents and licensing requirements can depend on jurisdiction and use; this notice is not a patent grant or legal advice.

## Build tools and retained Linux dependencies

[Inno Setup](https://jrsoftware.org/isinfo.php) compiles the Windows installer; see its [source](https://github.com/jrsoftware/issrc) and [license](https://github.com/jrsoftware/issrc/blob/main/license.txt) for installer-engine redistribution and build-tool terms. The full compiler is not installed as a DoubleTake runtime dependency.

Retained Linux capture/tooling can use [PipeWire](https://pipewire.org/), [xdg-desktop-portal](https://github.com/flatpak/xdg-desktop-portal), [PulseAudio](https://www.freedesktop.org/wiki/Software/PulseAudio/), X11 and KDE Plasma. These are system dependencies supplied under their own licenses, not part of the Windows payload. The [Plasma widget](plasmoid/README.md) and project man pages remain in source under the project's applicable license.

The installer and build scripts do not waive any third-party terms. If modifying dependencies, enabling optional codecs or redistributing a different runtime, update notices and include the exact applicable license/source obligations for that build.
