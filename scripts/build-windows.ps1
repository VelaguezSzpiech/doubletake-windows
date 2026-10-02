[CmdletBinding()]
param(
    [string]$Version = '1.0.0',
    [switch]$SkipInstaller,
    [string]$DotnetPath,
    [string]$ISCCPath
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Inno's numeric file version has four unsigned 16-bit components. Release
# versions deliberately use only major.minor.patch, without pre-release labels.
if ($Version -notmatch '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$') {
    throw 'Version must be a numeric major.minor.patch release version, for example 1.0.0.'
}
foreach ($part in $Version.Split('.')) {
    if ($part.Length -gt 5 -or [int]$part -gt 65535) {
        throw 'Each version component must be between 0 and 65535.'
    }
}
if ($env:OS -ne 'Windows_NT') { throw 'The Windows release must be built on Windows.' }

function Resolve-Tool([string]$ExplicitPath, [string]$CommandName, [string[]]$Candidates = @()) {
    if ($ExplicitPath) {
        if (-not (Test-Path -LiteralPath $ExplicitPath -PathType Leaf)) {
            throw "$CommandName executable not found at the specified path."
        }
        return (Resolve-Path -LiteralPath $ExplicitPath).Path
    }
    $command = Get-Command $CommandName -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($command) { return $command.Source }
    foreach ($candidate in $Candidates) {
        if (Test-Path -LiteralPath $candidate -PathType Leaf) { return $candidate }
    }
    throw "$CommandName not found. Install the required build tool or specify its executable path."
}

function Invoke-Checked([string]$Executable, [string[]]$Arguments) {
    & $Executable @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$(Split-Path $Executable -Leaf) exited with code $LASTEXITCODE." }
}

function Copy-LicenseFiles([string]$SourceDirectory, [string]$DestinationDirectory, [switch]$RequireNotices) {
    $files = @(Get-ChildItem -LiteralPath $SourceDirectory -File -Recurse | Where-Object {
        $_.Name -match '^(LICENSE|LICENCE|COPYING|NOTICE|THIRD[-_ ]?PARTY[-_ ]?NOTICES)(\..*|[-_ ].*|$)'
    })
    if (-not ($files | Where-Object { $_.Name -match '^(LICENSE|LICENCE|COPYING)(\..*|[-_ ].*|$)' })) {
        throw 'A dependency is missing its license text; refusing to create an incomplete release.'
    }
    if ($RequireNotices -and -not ($files | Where-Object { $_.Name -match '^THIRD[-_ ]?PARTY[-_ ]?NOTICES' })) {
        throw 'A resolved .NET runtime pack is missing its third-party notices.'
    }
    $root = $SourceDirectory.TrimEnd('\', '/')
    foreach ($file in $files) {
        $relative = $file.FullName.Substring($root.Length).TrimStart('\', '/')
        $destination = Join-Path $DestinationDirectory $relative
        New-Item -ItemType Directory -Path (Split-Path $destination -Parent) -Force | Out-Null
        Copy-Item -LiteralPath $file.FullName -Destination $destination
    }
}

function Copy-DesktopNotices([string]$PackDirectory, [string]$DestinationDirectory) {
    # The Desktop runtime pack ships LICENSE but omits the component notices.
    # Resolve immutable source revisions from its own NuGet provenance.
    $nuspec = Join-Path $PackDirectory 'microsoft.windowsdesktop.app.runtime.win-x64.nuspec'
    [xml]$metadata = Get-Content -LiteralPath $nuspec -Raw
    $revision = [string]$metadata.package.metadata.repository.commit
    if ($revision -notmatch '^[a-f0-9]{40}$') { throw 'Desktop runtime source revision is missing.' }
    $manifestUrl = "https://raw.githubusercontent.com/dotnet/windowsdesktop/$revision/eng/Version.Details.xml"
    [xml]$manifest = (Invoke-WebRequest -UseBasicParsing -Uri $manifestUrl -TimeoutSec 60).Content
    $components = @{
        wpf = 'Microsoft.DotNet.Wpf.GitHub'
        winforms = 'Microsoft.Private.Winforms'
    }
    foreach ($component in $components.Keys) {
        $dependency = @($manifest.Dependencies.ProductDependencies.Dependency | Where-Object { $_.Name -eq $components[$component] })
        if ($dependency.Count -ne 1) { throw "Cannot resolve $component notice provenance." }
        $commit = [string]$dependency[0].Sha
        if ($commit -notmatch '^[a-f0-9]{40}$') { throw "Invalid $component source revision." }
        $url = "https://raw.githubusercontent.com/dotnet/$component/$commit/THIRD-PARTY-NOTICES.TXT"
        $destination = Join-Path $DestinationDirectory "THIRD-PARTY-NOTICES-$component.TXT"
        Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $destination -TimeoutSec 60
        if ((Get-Item -LiteralPath $destination).Length -eq 0) { throw "Empty $component notices." }
        $url | Add-Content -LiteralPath (Join-Path $DestinationDirectory 'SOURCES.txt') -Encoding UTF8
    }
}

$sourceRoot = Split-Path $PSScriptRoot -Parent
$dist = Join-Path $sourceRoot 'dist/windows'
$app = Join-Path $dist 'app'
$project = Join-Path $sourceRoot 'windows-tray/DoubleTake.Tray.csproj'
$go = Resolve-Tool '' 'go.exe'
$dotnet = Resolve-Tool $DotnetPath 'dotnet.exe'
$iscc = $null
if (-not $SkipInstaller) {
    $iscc = Resolve-Tool $ISCCPath 'ISCC.exe' @(
        (Join-Path $env:LOCALAPPDATA 'Programs/Inno Setup 6/ISCC.exe'),
        (Join-Path ${env:ProgramFiles(x86)} 'Inno Setup 6/ISCC.exe'),
        (Join-Path $env:ProgramFiles 'Inno Setup 6/ISCC.exe')
    )
}

Push-Location $sourceRoot
$oldGoOS = $env:GOOS
$oldGoArch = $env:GOARCH
$oldCGO = $env:CGO_ENABLED
try {
    $goVersion = (Invoke-Checked $go @('env', 'GOVERSION') | Out-String).Trim()
    $requiredGo = [regex]::Match((Get-Content -LiteralPath (Join-Path $sourceRoot 'go.mod') -Raw), '(?m)^go\s+(\d+\.\d+(?:\.\d+)?)\s*$')
    if (-not $requiredGo.Success -or $goVersion -notmatch '^go(\d+\.\d+(?:\.\d+)?)$' -or
        [version]$Matches[1] -lt [version]$requiredGo.Groups[1].Value) {
        throw 'Install a stable Go toolchain at least as new as the go directive in go.mod.'
    }
    $sdkVersion = (Invoke-Checked $dotnet @('--version') | Out-String).Trim()
    if ($sdkVersion -notmatch '^8\.0\.\d+$') { throw 'Select a stable .NET 8 SDK with -DotnetPath.' }
    foreach ($name in @('LICENSE', 'COPYING.GPL', 'THIRD_PARTY_NOTICES.md')) {
        if (-not (Test-Path -LiteralPath (Join-Path $sourceRoot $name) -PathType Leaf)) {
            throw "Required release notice is missing: $name"
        }
    }

    # Only our generated release tree is removed; installed state and source are untouched.
    $distParent = Join-Path $sourceRoot 'dist'
    if ((Test-Path -LiteralPath $distParent) -and
        ((Get-Item -LiteralPath $distParent).Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw 'dist is a reparse point; remove it manually before rebuilding.'
    }
    if (Test-Path -LiteralPath $dist) {
        $links = @(Get-Item -LiteralPath $dist; Get-ChildItem -LiteralPath $dist -Recurse -Force) |
            Where-Object { $_.Attributes -band [IO.FileAttributes]::ReparsePoint }
        if ($links) { throw 'dist/windows contains a reparse point; remove it manually before rebuilding.' }
        Remove-Item -LiteralPath $dist -Recurse -Force
    }
    New-Item -ItemType Directory -Path $app -Force | Out-Null
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    Invoke-Checked $go @('build', '-trimpath', '-buildvcs=false', '-ldflags=-s -w', '-o', (Join-Path $app 'doubletake.exe'), './cmd/doubletake')
    # Incremental publish can reuse an earlier debug-bearing assembly even when
    # publication excludes PDB files. Clean this configuration before compiling.
    Invoke-Checked $dotnet @('clean', $project, '-c', 'Release', '-r', 'win-x64', '-p:SelfContained=true')
    Invoke-Checked $dotnet @('publish', $project, '-c', 'Release', '-r', 'win-x64', '--self-contained', 'true', '-o', $app,
        "-p:Version=$Version", '-p:DebugType=none', '-p:DebugSymbols=false', '-p:CopyOutputSymbolsToPublishDirectory=false', '-p:ContinuousIntegrationBuild=true', "-p:PathMap=$sourceRoot=/_/DoubleTake")
    $symbols = @(Get-ChildItem -LiteralPath $app -Recurse -File | Where-Object { $_.Extension -in @('.pdb', '.mdb') })
    if ($symbols) { $symbols | Remove-Item -Force }
    foreach ($name in @('LICENSE', 'COPYING.GPL', 'THIRD_PARTY_NOTICES.md')) {
        Copy-Item -LiteralPath (Join-Path $sourceRoot $name) -Destination (Join-Path $app $name)
    }

    $licenseIndex = [Collections.Generic.List[string]]::new()
    $licenseIndex.Add('Dependency licenses (module/package identifiers and versions; no build-machine paths).')
    $goRoot = (Invoke-Checked $go @('env', 'GOROOT') | Out-String).Trim()
    $goLicense = Join-Path $app 'licenses/go/toolchain/LICENSE'
    New-Item -ItemType Directory -Path (Split-Path $goLicense -Parent) -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $goRoot 'LICENSE') -Destination $goLicense
    $licenseIndex.Add("Go standard library ${goVersion}: go/toolchain/LICENSE")
    Invoke-Checked $go @('mod', 'download')
    $modules = @(Invoke-Checked $go @('list', '-m', '-f', '{{if not .Main}}{{.Path}}{{end}}', 'all'))
    foreach ($module in $modules) {
        if (-not $module.Trim()) { continue }
        $metadata = (Invoke-Checked $go @('list', '-m', '-json', $module) | Out-String) | ConvertFrom-Json
        if ($metadata.PSObject.Properties['Replace']) { throw 'Local/replaced Go modules are not supported for public release builds.' }
        if (-not $metadata.PSObject.Properties['Dir'] -or -not $metadata.Dir) {
            $metadata = (Invoke-Checked $go @('mod', 'download', '-json', "$($metadata.Path)@$($metadata.Version)") | Out-String) | ConvertFrom-Json
        }
        $relative = 'go/' + $metadata.Path + '@' + $metadata.Version
        Copy-LicenseFiles $metadata.Dir (Join-Path $app "licenses/$relative")
        $licenseIndex.Add("$($metadata.Path) $($metadata.Version): $relative")
    }

    # Resolve the exact packs selected by this SDK/restore, not a guessed cache version.
    $packData = (Invoke-Checked $dotnet @('msbuild', $project, '-nologo', '-verbosity:quiet', '-target:ResolveFrameworkReferences',
        '-property:Configuration=Release', '-property:RuntimeIdentifier=win-x64', '-property:SelfContained=true', '-getItem:ResolvedFrameworkReference') | Out-String) | ConvertFrom-Json
    $packs = @($packData.Items.ResolvedFrameworkReference)
    if ($packs.Count -lt 2) { throw 'Could not resolve the .NET and Windows Desktop runtime packs for licensing.' }
    foreach ($pack in $packs) {
        $id = $pack.RuntimePackName
        $packVersion = $pack.RuntimePackVersion
        if (-not $id -or -not $packVersion -or -not $pack.RuntimePackPath) { throw 'Resolved runtime pack metadata is incomplete.' }
        $relative = "dotnet/$id/$packVersion"
        $destination = Join-Path $app "licenses/$relative"
        if ($id -eq 'Microsoft.WindowsDesktop.App.Runtime.win-x64') {
            Copy-LicenseFiles $pack.RuntimePackPath $destination
            Copy-DesktopNotices $pack.RuntimePackPath $destination
        } else {
            Copy-LicenseFiles $pack.RuntimePackPath $destination -RequireNotices
        }
        $licenseIndex.Add("$id $packVersion`: $relative")
    }
    $licenseIndex | Set-Content -LiteralPath (Join-Path $app 'licenses/INDEX.txt') -Encoding UTF8
    foreach ($name in @('DoubleTake.Tray.exe', 'doubletake.exe', 'coreclr.dll', 'PresentationFramework.dll')) {
        if (-not (Test-Path -LiteralPath (Join-Path $app $name) -PathType Leaf)) { throw "Release payload is incomplete: $name" }
    }

    if (-not $SkipInstaller) {
        $dependency = Get-Content -LiteralPath (Join-Path $sourceRoot 'packaging/windows/dependencies.json') -Raw | ConvertFrom-Json
        $gst = $dependency.gstreamer
        if ($gst.sha256 -notmatch '^[a-f0-9]{64}$' -or $gst.url -notmatch '^https://gstreamer\.freedesktop\.org/' -or
            $gst.fileName -notmatch '^[A-Za-z0-9._-]+\.exe$') { throw 'Invalid pinned GStreamer dependency metadata.' }
        foreach ($file in $gst.requiredFiles) {
            if ($file -notmatch '^[A-Za-z0-9_./-]+$' -or $file -match '\.\.' -or $file.StartsWith('/')) { throw 'Invalid runtime prerequisite file name.' }
        }
        $includePath = Join-Path $dist 'dependencies.iss'
        @(
            ('#define GStreamerVersion "' + $gst.version + '"')
            ('#define GStreamerURL "' + $gst.url + '"')
            ('#define GStreamerSHA256 "' + $gst.sha256 + '"')
            ('#define GStreamerFileName "' + $gst.fileName + '"')
            ('#define GStreamerRequiredFiles "' + (($gst.requiredFiles | ForEach-Object { $_.Replace('/', '\') }) -join '|') + '"')
        ) | Set-Content -LiteralPath $includePath -Encoding UTF8
        Invoke-Checked $iscc @("/DAppVersion=$Version", "/DPayloadDir=$app", "/DReleaseDir=$dist",
            "/DDependencyInclude=$includePath", (Join-Path $sourceRoot 'packaging/windows/installer.iss'))
        $installerName = "DoubleTake-Windows-$Version-Setup.exe"
        $installer = Join-Path $dist $installerName
        if (-not (Test-Path -LiteralPath $installer -PathType Leaf)) { throw 'Inno Setup did not produce the expected installer.' }
        $hash = (Get-FileHash -LiteralPath $installer -Algorithm SHA256).Hash.ToLowerInvariant()
        "$hash  $installerName" | Set-Content -LiteralPath (Join-Path $dist 'SHA256SUMS.txt') -Encoding ASCII
        Write-Host "Installer: dist/windows/$installerName"
        Write-Host 'Checksums: dist/windows/SHA256SUMS.txt'
    }
    Write-Host 'App payload: dist/windows/app (launch DoubleTake.Tray.exe)'
}
finally {
    $env:GOOS = $oldGoOS
    $env:GOARCH = $oldGoArch
    $env:CGO_ENABLED = $oldCGO
    Pop-Location
}
