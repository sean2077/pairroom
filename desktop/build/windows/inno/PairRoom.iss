; Wails compiles the host and CLI; Inno owns Windows installation.
#if VER < EncodeVer(7, 0, 0)
  #error Inno Setup 7 or later is required
#endif
#ifndef PairRoomVersion
  #error Build with scripts/package-windows.py so VERSION is validated
#endif
#ifndef PairRoomArch
  #error PairRoomArch must be amd64 or arm64
#endif
#define DesktopRoot AddBackslash(SourcePath) + "..\..\.."
#define PairRoomId "com.sean2077.pairroom.desktop"

[Setup]
AppId={#PairRoomId}
AppName=PairRoom
AppVersion={#PairRoomVersion}
AppPublisher=PairRoom contributors
AppPublisherURL=https://github.com/sean2077/pairroom
AppSupportURL=https://github.com/sean2077/pairroom/issues
AppUpdatesURL=https://github.com/sean2077/pairroom/releases
UninstallDisplayName=PairRoom
UninstallDisplayIcon={app}\PairRoom.exe
VersionInfoVersion={#PairRoomVersion}.0
DefaultDirName={autopf}\PairRoom contributors\PairRoom
; Retain the existing machine scope. Switching scope is a separate migration.
PrivilegesRequired=admin
MinVersion=10.0
#if PairRoomArch == "amd64"
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
#elif PairRoomArch == "arm64"
ArchitecturesAllowed=arm64
ArchitecturesInstallIn64BitMode=arm64
#else
  #error Unsupported PairRoomArch
#endif
WizardStyle=modern dynamic windows11
SetupIconFile={#DesktopRoot}\build\windows\icon.ico
DisableWelcomePage=yes
DisableProgramGroupPage=yes
DisableDirPage=auto
DisableReadyPage=yes
UsePreviousAppDir=yes
Compression=lzma2
SolidCompression=yes
SetupLogging=yes
; PairRoom must drain active Turns itself, not be closed by Restart Manager.
CloseApplications=no
RestartApplications=no
ChangesEnvironment=no

[Files]
Source: "{#DesktopRoot}\bin\PairRoom.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#DesktopRoot}\bin\cli\pairroom.exe"; DestDir: "{app}\bin"; Flags: ignoreversion
Source: "{#DesktopRoot}\..\LICENSE"; DestDir: "{app}"; DestName: "LICENSE.txt"; Flags: ignoreversion
Source: "MicrosoftEdgeWebview2Setup.exe"; Flags: dontcopy

[Icons]
Name: "{commonprograms}\PairRoom"; Filename: "{app}\PairRoom.exe"

[Run]
Filename: "{app}\PairRoom.exe"; Description: "Launch PairRoom"; Flags: postinstall nowait skipifsilent runasoriginaluser

[Code]
const
  UninstallRoot = 'Software\Microsoft\Windows\CurrentVersion\Uninstall';
  InnoKey = '{#PairRoomId}_is1';
  WebViewKey = 'Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}';
  RunKey = 'Software\Microsoft\Windows\CurrentVersion\Run';
  { Unattended provisioning budget: silent runs never wait indefinitely on the
    Evergreen download (winget validation starves it and times the install out). }
  WebView2SilentWaitMs = 300000;
  WebView2PollIntervalMs = 2000;

procedure Sleep(Milliseconds: DWORD);
  external 'Sleep@kernel32.dll stdcall';

function CreateFile(Name: String; Access, Share: Cardinal; Security: NativeInt;
  Creation, Flags: Cardinal; Template: NativeInt): NativeInt;
  external 'CreateFileW@kernel32.dll stdcall';
function CloseHandle(Handle: NativeInt): Boolean;
  external 'CloseHandle@kernel32.dll stdcall';

function HasOtherInstaller(Hive: Integer): Boolean;
var
  Keys: TArrayOfString;
  I: Integer;
  Name, Publisher: String;
begin
  Result := False;
  if not RegGetSubkeyNames(Hive, UninstallRoot, Keys) then exit;
  for I := 0 to GetArrayLength(Keys) - 1 do
    if not SameText(Keys[I], InnoKey) and
       RegQueryStringValue(Hive, UninstallRoot + '\' + Keys[I], 'DisplayName', Name) and
       RegQueryStringValue(Hive, UninstallRoot + '\' + Keys[I], 'Publisher', Publisher) and
       SameText(Name, 'PairRoom') and SameText(Publisher, 'PairRoom contributors') then
    begin
      Result := True;
      exit;
    end;
end;

function InitializeSetup: Boolean;
begin
  { Never run an untrusted registry command or leave an old recursive uninstaller
    pointing at the new payload. This is a one-time, explicit installer change. }
  Result := not (HasOtherInstaller(HKLM64) or HasOtherInstaller(HKLM32) or
                 HasOtherInstaller(HKCU64) or HasOtherInstaller(HKCU32));
  if not Result then
  begin
    Log('An older PairRoom installer is registered.');
    SuppressibleMsgBox('An older PairRoom installer is registered. Quit PairRoom '
      + 'from the tray, gracefully stop any installed daemon, and uninstall the '
      + 'old version from Windows Settings before running this installer. '
      + 'Back up your Service data first. This installer has not changed it.',
      mbError, MB_OK, IDOK);
  end;
end;

function BinaryWritable(const Name: String): Boolean;
var
  Handle: NativeInt;
begin
  Result := True;
  if not FileExists(Name) then exit;
  { OPEN_EXISTING + GENERIC_WRITE, without truncating or changing the file.
    A mapped/running executable cannot be opened for writing on Windows. }
  Handle := CreateFile(Name, $40000000, 7, 0, 3, 0, 0);
  Result := Handle <> -1;
  if Result then CloseHandle(Handle);
end;

function PayloadError: String;
begin
  Result := '';
  if not BinaryWritable(ExpandConstant('{app}\PairRoom.exe')) or
     not BinaryWritable(ExpandConstant('{app}\bin\pairroom.exe')) then
    Result := 'PairRoom files are in use or not writable. Quit PairRoom from its '
      + 'tray and gracefully stop any daemon using the bundled CLI, then retry. '
      + 'No process will be terminated by this installer.';
end;

function HasWebView2: Boolean;
var
  Version: String;
  PackedVersion: Int64;
begin
  { Machine installation must not rely on the elevating user's private runtime. }
  Result := RegQueryStringValue(HKLM32, WebViewKey, 'pv', Version) and
    StrToVersion(Version, PackedVersion) and (PackedVersion <> 0);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  ExitCode: Integer;
  Waited: Integer;
begin
  Result := PayloadError;
  if Result <> '' then
  begin
    Log(Result);
    exit;
  end;
  if FileExists(ExpandConstant('{app}\uninstall.exe')) then
  begin
    Result := 'The selected folder still contains an older PairRoom uninstaller. '
      + 'Remove the old installation first; do not overwrite it.';
    exit;
  end;
  if not HasWebView2 then
  begin
    ExtractTemporaryFile('MicrosoftEdgeWebview2Setup.exe');
    if WizardSilent then
    begin
      { Start the bootstrapper detached and poll the machine-wide runtime
        registration with a bounded budget: an unattended run whose download
        is blocked or starved must fail with actionable guidance instead of
        hanging until the caller's own timeout (the winget validation verdict
        for the 5.0.1 submission). A registered runtime means the bootstrap
        succeeded; its hidden process then winds down on its own. }
      if not Exec(ExpandConstant('{tmp}\MicrosoftEdgeWebview2Setup.exe'),
        '/silent /install', '', SW_HIDE, ewNoWait, ExitCode) then
        Result := 'Could not start the Microsoft WebView2 runtime installer.'
      else
      begin
        Waited := 0;
        while (Waited < WebView2SilentWaitMs) and not HasWebView2 do
        begin
          Sleep(WebView2PollIntervalMs);
          Waited := Waited + WebView2PollIntervalMs;
        end;
        if not HasWebView2 then
        begin
          Result := 'Microsoft WebView2 runtime was not provisioned within '
            + IntToStr(WebView2SilentWaitMs div 1000) + ' seconds of silent '
            + 'installation. Provision the runtime first (for example: winget '
            + 'install Microsoft.EdgeWebView2Runtime) or run Setup '
            + 'interactively. PairRoom has not been installed.';
          Log(Result);
        end;
      end;
    end
    else if not Exec(ExpandConstant('{tmp}\MicrosoftEdgeWebview2Setup.exe'),
      '/silent /install', '', SW_HIDE, ewWaitUntilTerminated, ExitCode) then
      Result := 'Could not start the Microsoft WebView2 runtime installer.'
    else if ExitCode = 3010 then
    begin
      NeedsRestart := True;
      Result := 'WebView2 requires a restart. Restart Windows and run PairRoom Setup again.';
    end
    else if (ExitCode <> 0) or not HasWebView2 then
      Result := 'Microsoft WebView2 installation failed (exit code '
        + IntToStr(ExitCode) + '). Check network access or install the Evergreen '
        + 'Runtime from Microsoft, then retry. PairRoom has not been installed.';
  end;
end;

function InitializeUninstall: Boolean;
var
  Error: String;
begin
  Error := PayloadError;
  Result := Error = '';
  if not Result then
  begin
    Log(Error);
    SuppressibleMsgBox(Error, mbError, MB_OK, IDOK);
  end;
end;

procedure RemoveOwnedStartupEntry(Hive: Integer);
var
  Command, Target: String;
begin
  Target := ExpandConstant('{app}\PairRoom.exe');
  if RegQueryStringValue(Hive, RunKey, 'PairRoom', Command) and
     (SameText(Command, Target) or SameText(Command, '"' + Target + '"')) then
    RegDeleteValue(Hive, RunKey, 'PairRoom');
end;

procedure CurUninstallStepChanged(Step: TUninstallStep);
begin
  if Step = usPostUninstall then
  begin
    RemoveOwnedStartupEntry(HKCU64);
    RemoveOwnedStartupEntry(HKCU32);
  end;
  { No recursive directory deletion, data purge, PATH edits, or daemon commands.
    Inno removes only logged payload files/shortcuts; unrelated files survive. }
end;
