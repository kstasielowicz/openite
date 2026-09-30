; Openite Windows installer (Inno Setup 6).  Build:  iscc /DVersion=0.3.0 installer\openite.iss   (after ./build.sh)
; Per-user install: no administrator rights, nothing outside your profile, fully removed by the uninstaller.
#ifndef Version
  #define Version "0.0.0-dev"
#endif

[Setup]
AppId={{6E2B1D0A-7F53-4C6B-9C0E-A10E17E00001}
AppName=Openite
AppVersion={#Version}
AppPublisher=Openite contributors
AppPublisherURL=https://github.com/kstasielowicz/openite
DefaultDirName={localappdata}\Programs\Openite
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
ArchitecturesInstallIn64BitMode=x64compatible
LicenseFile=..\LICENSE
OutputDir=..\dist
OutputBaseFilename=openite-setup-{#Version}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
UninstallDisplayIcon={app}\openite.exe

[Tasks]
Name: "addtopath"; Description: "Add Openite to my PATH, so I can type ""openite"" in any terminal"; Flags: checkedonce

[Files]
Source: "..\dist\openite-windows-amd64.exe"; DestName: "openite.exe"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\Openite"; Filename: "{app}\openite.exe"; Parameters: "ui"; Comment: "Install and update your apps"

[Registry]
Root: HKCU; Subkey: "Environment"; ValueType: expandsz; ValueName: "Path"; ValueData: "{olddata};{app}"; Tasks: addtopath; Check: NeedsAddPath(ExpandConstant('{app}'))

[Run]
Filename: "{app}\openite.exe"; Parameters: "ui"; Description: "Open Openite now"; Flags: postinstall nowait skipifsilent

[Code]
function NeedsAddPath(Dir: string): Boolean;
var P: string;
begin
  if not RegQueryStringValue(HKCU, 'Environment', 'Path', P) then P := '';
  Result := Pos(';' + Lowercase(Dir) + ';', ';' + Lowercase(P) + ';') = 0;
end;

procedure CurUninstallStepChanged(Step: TUninstallStep);
var P, Dir: string; I: Integer;
begin
  if Step = usPostUninstall then begin
    Dir := ExpandConstant('{app}');
    if RegQueryStringValue(HKCU, 'Environment', 'Path', P) then begin
      I := Pos(';' + Lowercase(Dir), ';' + Lowercase(P));
      if I > 0 then begin
        Delete(P, I, Length(Dir) + 1);
        RegWriteExpandStringValue(HKCU, 'Environment', 'Path', P);
      end;
    end;
  end;
end;
