#ifndef AgentPath
  #error Specify AgentPath using the build script.
#endif
#ifndef AppVersion
  #error Specify AppVersion using the build script.
#endif
#define LegacyKey "Software\Microsoft\Windows\CurrentVersion\Uninstall\CodexRM"
#define RunKey "Software\Microsoft\Windows\CurrentVersion\Run"

[Setup]
AppId=CodexRM.Agent
AppName=CodexRM
AppVersion={#AppVersion}
AppPublisher=CodexRM
DefaultDirName={code:DefaultDirectory}
DefaultGroupName=CodexRM
DisableWelcomePage=no
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
SetupMutex=CodexRM.Agent.Setup
SetupIconFile=..\..\internal\tray\icon.ico
UninstallDisplayIcon={app}\bin\codexrm-agent.exe
VersionInfoVersion={#AppVersion}
VersionInfoProductName=CodexRM
VersionInfoDescription=CodexRM Agent Setup
OutputBaseFilename=CodexRM-Setup
Compression=lzma2
SolidCompression=yes
WizardStyle=modern dynamic
CloseApplications=no

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"
Name: "chinesesimplified"; MessagesFile: "compiler:Languages\ChineseSimplified.isl"

[Tasks]
Name: "autostart"; Description: "{cm:AutoStartAgent}"; Check: DefaultAutostart
Name: "autostart"; Description: "{cm:AutoStartAgent}"; Flags: unchecked; Check: not DefaultAutostart
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; Flags: unchecked

[CustomMessages]
AutoStartAgent=Start CodexRM when I sign in to Windows
LaunchAgent=Launch CodexRM
ExistingInstall=Use the existing CodexRM installation directory to upgrade it.
ActionFailed=Could not manage the CodexRM Agent. Check the setup log for details.
chinesesimplified.AutoStartAgent=登录 Windows 时启动 CodexRM
chinesesimplified.LaunchAgent=启动 CodexRM
chinesesimplified.ExistingInstall=请使用现有 CodexRM 安装目录进行升级。
chinesesimplified.ActionFailed=无法操作 CodexRM Agent，请查看安装日志。

[Files]
Source: "{#AgentPath}"; DestDir: "{app}\bin"; DestName: "codexrm-agent.exe"; Flags: ignoreversion; AfterInstall: RegisterAgent

[Icons]
Name: "{group}\CodexRM"; Filename: "{app}\bin\codexrm-agent.exe"; Parameters: "--settings"; WorkingDir: "{app}"
Name: "{userdesktop}\CodexRM"; Filename: "{app}\bin\codexrm-agent.exe"; Parameters: "--settings"; WorkingDir: "{app}"; Tasks: desktopicon

[Registry]
Root: HKCU; Subkey: "{#RunKey}"; ValueType: string; ValueName: "CodexRM"; ValueData: """{app}\bin\codexrm-agent.exe"""; Flags: uninsdeletevalue; Tasks: autostart
Root: HKCU; Subkey: "{#RunKey}"; ValueName: "CodexRM"; Flags: deletevalue; Tasks: not autostart

[InstallDelete]
Type: files; Name: "{userdesktop}\CodexRM.lnk"; Tasks: not desktopicon; Check: IsExistingInstall

[UninstallDelete]
Type: files; Name: "{app}\.state\installation.json"
Type: dirifempty; Name: "{app}\.state"
Type: dirifempty; Name: "{app}"

[Run]
Filename: "{app}\bin\codexrm-agent.exe"; WorkingDir: "{app}"; Description: "{cm:LaunchAgent}"; Flags: postinstall nowait skipifsilent

[Code]
var
  LegacyDir: String;

function InitializeSetup: Boolean;
begin
  if RegQueryStringValue(HKCU, '{#LegacyKey}', 'InstallLocation', LegacyDir) then begin
    LegacyDir := RemoveBackslashUnlessRoot(LegacyDir);
    if not FileExists(AddBackslash(LegacyDir) + 'bin\codexrm-agent.exe') then
      LegacyDir := '';
  end;
  Result := True;
end;

function DefaultDirectory(Param: String): String;
begin
  if LegacyDir <> '' then
    Result := LegacyDir
  else
    Result := ExpandConstant('{localappdata}\CodexRM');
end;

function IsExistingInstall: Boolean;
begin
  Result := FileExists(ExpandConstant('{app}\bin\codexrm-agent.exe'));
end;

function DefaultAutostart: Boolean;
var
  RunValue: String;
begin
  Result := (LegacyDir = '') or
    (RegQueryStringValue(HKCU, '{#RunKey}', 'CodexRM', RunValue) and
      (CompareText(RemoveQuotes(RunValue), AddBackslash(LegacyDir) + 'bin\codexrm-agent.exe') = 0)) or
    FileExists(ExpandConstant('{userstartup}\CodexRM.lnk'));
end;

function RunAgentAction(const Binary, Action: String): String;
var
  Params: String;
  ExitCode: Integer;
  Output: TExecOutput;
begin
  Result := CustomMessage('ActionFailed');
  Params := '--setup-action ' + Action + ' --setup-dir ' +
    AddQuotes(RemoveBackslashUnlessRoot(ExpandConstant('{app}')));
  if (Action = 'register') and WizardIsTaskSelected('autostart') then
    Params := Params + ' --setup-autostart';
  if ExecAndCaptureOutput(Binary, Params, '', SW_HIDE, ewWaitUntilTerminated, ExitCode, Output) then begin
    if ExitCode = 0 then
      Result := ''
    else if GetArrayLength(Output.StdErr) > 0 then begin
      Result := Output.StdErr[0];
      Log(Result);
    end;
  end else
    Log('Agent action ' + Action + ' could not start. Error ' + IntToStr(ExitCode));
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  if (LegacyDir <> '') and
    (CompareText(LegacyDir, RemoveBackslashUnlessRoot(ExpandConstant('{app}'))) <> 0) then begin
    Result := CustomMessage('ExistingInstall') + #13#10 + LegacyDir;
    Exit;
  end;
  ExtractTemporaryFile('codexrm-agent.exe');
  Result := RunAgentAction(ExpandConstant('{tmp}\codexrm-agent.exe'), 'prepare');
end;

procedure RegisterAgent;
var
  ErrorMessage: String;
begin
  ErrorMessage := RunAgentAction(ExpandConstant('{app}\bin\codexrm-agent.exe'), 'register');
  if ErrorMessage <> '' then
    RaiseException(ErrorMessage);
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if (CurStep = ssPostInstall) and (LegacyDir <> '') then begin
    RegDeleteKeyIncludingSubkeys(HKCU, '{#LegacyKey}');
    DeleteFile(ExpandConstant('{userstartup}\CodexRM.lnk'));
    DeleteFile(ExpandConstant('{userdesktop}\CodexRM 重启服务.lnk'));
    DeleteFile(ExpandConstant('{userdesktop}\CodexRM 设置和生成配对码.lnk'));
    DeleteFile(ExpandConstant('{userdesktop}\CodexRM Pair.lnk'));
    DeleteFile(ExpandConstant('{userdesktop}\CodexRM Settings.lnk'));
  end;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  ErrorMessage: String;
begin
  if (CurUninstallStep = usUninstall) and IsExistingInstall then begin
    ErrorMessage := RunAgentAction(ExpandConstant('{app}\bin\codexrm-agent.exe'), 'unregister');
    if ErrorMessage <> '' then
      RaiseException(ErrorMessage);
  end;
end;
