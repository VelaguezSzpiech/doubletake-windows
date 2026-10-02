; Compile through scripts/build-windows.ps1 so dependency pins and licensing
; are resolved from the source manifests, not from private workstation paths.
#ifndef AppVersion
  #error Build with scripts/build-windows.ps1 and a numeric release version.
#endif
#ifndef PayloadDir
  #error PayloadDir is required.
#endif
#ifndef ReleaseDir
  #error ReleaseDir is required.
#endif
#ifndef DependencyInclude
  #error DependencyInclude is required.
#endif
#include DependencyInclude

[Setup]
AppId={{57A967D4-A798-4A17-8A49-AE50A82E71F3}
AppName=DoubleTake for Windows
AppVersion={#AppVersion}
AppVerName=DoubleTake for Windows {#AppVersion}
AppPublisher=DoubleTake Windows contributors (unofficial port)
AppPublisherURL=https://github.com/VelaguezSzpiech/doubletake-windows
AppSupportURL=https://github.com/VelaguezSzpiech/doubletake-windows/issues
AppUpdatesURL=https://github.com/VelaguezSzpiech/doubletake-windows/releases
VersionInfoVersion={#AppVersion}.0
DefaultDirName={localappdata}\Programs\DoubleTake
DefaultGroupName=DoubleTake
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
OutputDir={#ReleaseDir}
OutputBaseFilename=DoubleTake-Windows-{#AppVersion}-Setup
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
UninstallDisplayIcon={app}\DoubleTake.Tray.exe
UninstallDisplayName=DoubleTake for Windows
LicenseFile={#PayloadDir}\LICENSE
CloseApplications=yes
CloseApplicationsFilter=*.exe,*.dll
RestartApplications=no
SetupMutex=DoubleTakeWindowsSetup

[Tasks]
Name: "desktopicon"; Description: "Create a &desktop shortcut"; GroupDescription: "Shortcuts:"; Flags: unchecked

[Files]
Source: "{#PayloadDir}\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs; Excludes: "*.pdb,*.mdb,*.log,*.dmp"

[Icons]
Name: "{userprograms}\DoubleTake"; Filename: "{app}\DoubleTake.Tray.exe"; WorkingDir: "{app}"; AppUserModelID: "DoubleTake.Windows"
Name: "{userdesktop}\DoubleTake"; Filename: "{app}\DoubleTake.Tray.exe"; WorkingDir: "{app}"; AppUserModelID: "DoubleTake.Windows"; Tasks: desktopicon

[Run]
Filename: "{app}\DoubleTake.Tray.exe"; Description: "Launch &DoubleTake"; WorkingDir: "{app}"; Flags: nowait postinstall skipifsilent unchecked

[Code]
var
  DownloadPage: TDownloadWizardPage;

function CreateFileForWrite(FileName: String; DesiredAccess, ShareMode: Cardinal;
  SecurityAttributes: Integer; CreationDisposition, FlagsAndAttributes: Cardinal;
  TemplateFile: Integer): Integer;
  external 'CreateFileW@kernel32.dll stdcall';
function CloseFileHandle(Handle: Integer): Boolean;
  external 'CloseHandle@kernel32.dll stdcall';

function RuntimeHasRequiredFiles(const Root: String): Boolean;
var
  Remaining, RelativeName: String;
  Separator: Integer;
begin
  Result := False;
  if Root = '' then exit;
  Remaining := '{#GStreamerRequiredFiles}';
  while Remaining <> '' do begin
    Separator := Pos('|', Remaining);
    if Separator = 0 then begin
      RelativeName := Remaining;
      Remaining := '';
    end else begin
      RelativeName := Copy(Remaining, 1, Separator - 1);
      Delete(Remaining, 1, Separator);
    end;
    if not FileExists(AddBackslash(Root) + RelativeName) then exit;
  end;
  Result := True;
end;

function FindCompleteRuntime: Boolean;
var
  Candidates: TArrayOfString;
  Index: Integer;
begin
  SetArrayLength(Candidates, 4);
  Candidates[0] := GetEnv('GSTREAMER_1_0_ROOT_MSVC_X86_64');
  Candidates[1] := ExpandConstant('{localappdata}\Programs\gstreamer\1.0\msvc_x86_64');
  Candidates[2] := ExpandConstant('{commonpf64}\gstreamer\1.0\msvc_x86_64');
  Candidates[3] := 'C:\gstreamer\1.0\msvc_x86_64';
  Result := False;
  for Index := 0 to GetArrayLength(Candidates) - 1 do begin
    // Match the backend search order: an incomplete higher-priority runtime
    // must not shadow the newly installed per-user prerequisite unnoticed.
    if (Candidates[Index] <> '') and
      FileExists(AddBackslash(Candidates[Index]) + 'bin\gst-launch-1.0.exe') then begin
      Result := RuntimeHasRequiredFiles(Candidates[Index]);
      exit;
    end;
  end;
end;

procedure InitializeWizard;
begin
  DownloadPage := CreateDownloadPage('GStreamer prerequisite',
    'Downloading the official GStreamer {#GStreamerVersion} Windows installer and verifying SHA-256.', nil);
  DownloadPage.ShowBaseNameInsteadOfUrl := True;
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  ExitCode: Integer;
begin
  Result := '';
  if FindCompleteRuntime then exit;
  try
    DownloadPage.Clear;
    DownloadPage.Add('{#GStreamerURL}', '{#GStreamerFileName}', '{#GStreamerSHA256}');
    DownloadPage.Show;
    try
      DownloadPage.Download;
    finally
      DownloadPage.Hide;
    end;
    // The download page refuses mismatched hashes. Verify again immediately
    // before executing, including when the temporary file was already cached.
    if CompareText(GetSHA256OfFile(ExpandConstant('{tmp}\{#GStreamerFileName}')),
      '{#GStreamerSHA256}') <> 0 then begin
      Result := 'GStreamer SHA-256 verification failed. No prerequisite was executed.';
      exit;
    end;
    if not Exec(ExpandConstant('{tmp}\{#GStreamerFileName}'),
      '/CURRENTUSER /VERYSILENT /SUPPRESSMSGBOXES /NORESTART', '', SW_HIDE,
      ewWaitUntilTerminated, ExitCode) then begin
      Result := 'Could not start the verified GStreamer installer. DoubleTake installation has stopped.';
      exit;
    end;
    if ExitCode <> 0 then begin
      Result := Format('GStreamer installation failed (exit code %d). DoubleTake installation has stopped.', [ExitCode]);
      exit;
    end;
    if not FindCompleteRuntime then
      Result := 'GStreamer installation did not provide a complete runtime. Repair the runtime or remove an incomplete higher-priority GSTREAMER_1_0_ROOT_MSVC_X86_64 setting, then run Setup again.';
  except
    Result := 'GStreamer prerequisite download or installation failed. An internet connection is required when the runtime is missing. ' + GetExceptionMessage;
  end;
end;

function CanUpdateExecutable(const Name: String): Boolean;
var
  Handle: Integer;
  FileName: String;
begin
  FileName := ExpandConstant('{app}\') + Name;
  Result := True;
  if not FileExists(FileName) then exit;
  // A running executable cannot be opened for writing. Restart Manager has
  // already had its opportunity to close the application before ssInstall.
  Handle := CreateFileForWrite(FileName, $40000000, 7, 0, 3, 0, 0);
  Result := Handle <> -1;
  if Result then CloseFileHandle(Handle);
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssInstall then begin
    if not CanUpdateExecutable('DoubleTake.Tray.exe') or not CanUpdateExecutable('doubletake.exe') then
      RaiseException('DoubleTake is still running or its executable cannot be updated. Quit DoubleTake from its tray menu, check installation-folder write access, and run Setup again. No application files have been replaced.');
  end;
end;

function InitializeUninstall: Boolean;
begin
  Result := CanUpdateExecutable('DoubleTake.Tray.exe') and CanUpdateExecutable('doubletake.exe');
  if not Result then
    SuppressibleMsgBox('Quit DoubleTake from its tray menu before uninstalling. If it is already closed, check installation-folder write access and try again. Your settings and saved pairing will not be removed.', mbError, MB_OK, IDOK);
end;
