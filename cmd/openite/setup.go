package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/openite/openite/catalog"
)

// Built-in Windows setup actions (catalog/setup.json): tweaks, optional features and Windows Update.
// Only ids travel over the network; the PowerShell comes from the catalog embedded in this binary.

// psPrelude defines the helpers the catalog snippets use.
//   - Set-UserReg applies a per-user setting to the current user, or, when running as SYSTEM (the agent service),
//     to every signed-in user and to the default profile that new users are created from.
//   - Invoke-WU drives the Windows Update Agent COM API: search, download, install, report.
const psPrelude = `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
function Set-RegValue($root, $path, $name, $value, $type) {
  if (-not $type) { $type = 'DWord' }
  $full = "$root\$path"
  if (-not (Test-Path $full)) { New-Item -Path $full -Force | Out-Null }
  if ($name -eq '') { Set-Item -Path $full -Value $value | Out-Null } else { New-ItemProperty -Path $full -Name $name -Value $value -PropertyType $type -Force | Out-Null }
}
function Set-MachineReg($path, $name, $value, $type) { Set-RegValue 'HKLM:' $path $name $value $type; Write-Output "  set HKLM\$path\$name = $value" }
function Set-UserReg($path, $name, $value, $type) {
  $me = [Security.Principal.WindowsIdentity]::GetCurrent()
  if (-not $me.IsSystem) { Set-RegValue 'HKCU:' $path $name $value $type; Write-Output "  set $path\$name = $value (current user)"; return }
  $n = 0
  foreach ($sid in (Get-ChildItem 'Registry::HKEY_USERS' | Where-Object { $_.PSChildName -match '^S-1-5-21-[\d-]+$' })) {
    Set-RegValue "Registry::HKEY_USERS\$($sid.PSChildName)" $path $name $value $type; $n++
  }
  $hive = "$env:SystemDrive\Users\Default\NTUSER.DAT"
  if (Test-Path $hive) {
    reg.exe load 'HKU\OpeniteDefault' $hive | Out-Null
    try { Set-RegValue 'Registry::HKEY_USERS\OpeniteDefault' $path $name $value $type } finally { [gc]::Collect(); reg.exe unload 'HKU\OpeniteDefault' | Out-Null }
  }
  Write-Output "  set $path\$name = $value ($n signed-in user(s) and new users)"
}
function Enable-Feature($name) {
  $f = Get-WindowsOptionalFeature -Online -FeatureName $name -ErrorAction SilentlyContinue
  if (-not $f) { throw "This edition of Windows does not have the feature $name." }
  if ($f.State -eq 'Enabled') { Write-Output "  $name is already on"; return }
  $r = Enable-WindowsOptionalFeature -Online -FeatureName $name -All -NoRestart
  Write-Output "  turned on $name"; if ($r.RestartNeeded) { Write-Output 'RESTART-NEEDED' }
}
function Disable-Feature($name) {
  $f = Get-WindowsOptionalFeature -Online -FeatureName $name -ErrorAction SilentlyContinue
  if (-not $f -or $f.State -ne 'Enabled') { Write-Output "  $name is already off"; return }
  $r = Disable-WindowsOptionalFeature -Online -FeatureName $name -NoRestart
  Write-Output "  turned off $name"; if ($r.RestartNeeded) { Write-Output 'RESTART-NEEDED' }
}
function Invoke-WU([string]$Filter, [string[]]$KBs, [switch]$ScanOnly) {
  $session = New-Object -ComObject Microsoft.Update.Session
  Write-Output 'Searching Windows Update...'
  $found = $session.CreateUpdateSearcher().Search("IsInstalled=0 and IsHidden=0 and Type='Software'").Updates
  $pick = New-Object -ComObject Microsoft.Update.UpdateColl
  foreach ($u in $found) {
    $kb = @($u.KBArticleIDs | ForEach-Object { "KB$_" })
    $cats = @($u.Categories | ForEach-Object { $_.Name })
    $ok = switch ($Filter) { 'security' { ($cats -contains 'Security Updates') -or ($cats -contains 'Critical Updates') } 'kb' { @($kb | Where-Object { $KBs -contains $_ }).Count -gt 0 } default { $true } }
    if ($ok) { [void]$pick.Add($u); Write-Output ("  - {0}" -f $u.Title) }
  }
  if ($Filter -eq 'kb') { foreach ($k in $KBs) { if (-not (@($pick | ForEach-Object { $_.KBArticleIDs | ForEach-Object { "KB$_" } }) -contains $k)) { Write-Output "  $k is not offered to this PC (already installed, superseded or not applicable)" } } }
  if ($pick.Count -eq 0) { Write-Output 'Nothing to install. This PC is up to date.'; return }
  if ($ScanOnly) { Write-Output ("{0} update(s) available." -f $pick.Count); return }
  foreach ($u in $pick) { if (-not $u.EulaAccepted) { $u.AcceptEula() } }
  Write-Output ("Downloading {0} update(s)..." -f $pick.Count)
  $dl = $session.CreateUpdateDownloader(); $dl.Updates = $pick; [void]$dl.Download()
  Write-Output 'Installing...'
  $in = $session.CreateUpdateInstaller(); $in.Updates = $pick; $res = $in.Install()
  for ($i = 0; $i -lt $pick.Count; $i++) {
    $c = $res.GetUpdateResult($i).ResultCode
    Write-Output ("  {0} {1}" -f $(if ($c -eq 2) { 'installed' } elseif ($c -eq 3) { 'installed with errors' } else { 'FAILED' }), $pick.Item($i).Title)
  }
  if ($res.RebootRequired) { Write-Output 'RESTART-NEEDED' }
  if ($res.ResultCode -ne 2 -and $res.ResultCode -ne 3) { throw ("Windows Update finished with result code {0}." -f $res.ResultCode) }
}
`

// setupScript builds the full PowerShell for one action. args are KB numbers and must already be validated.
func setupScript(it catalog.Setup, args []string) string {
	var b strings.Builder
	b.WriteString(psPrelude)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + a + "'"
	}
	b.WriteString("$kbArgs = @(" + strings.Join(quoted, ",") + ")\n")
	b.WriteString("try {\n")
	for _, l := range it.PS {
		b.WriteString("  " + l + "\n")
	}
	b.WriteString("} catch { Write-Output (\"error: \" + $_.Exception.Message); exit 1 }\n")
	return b.String()
}

// runSetup runs one setup step. It reports a restart hint through the returned output when Windows asks for one.
func runSetup(s Step, onLine func(string)) (int, string) {
	it, ok := catalog.FindSetup(s.Key)
	if !ok {
		return 1, "unknown setup action " + s.Key
	}
	for _, a := range s.Args {
		if !catalog.SafeKB.MatchString(a) {
			return 1, fmt.Sprintf("refusing unsafe KB number %q", a)
		}
	}
	if it.Arg == "kb" && len(s.Args) == 0 {
		return 1, "no KB numbers given"
	}
	if os.Getenv("OPENITE_DEMO") != "" || dryRun {
		return demoSetup(it, s.Args, onLine)
	}
	if runtime.GOOS != "windows" {
		return 1, it.Name + " is only available on Windows"
	}
	code, out := runScript(Step{Shell: "powershell", Body: setupScript(it, s.Args)}, onLine)
	if it.Kind == "update" {
		go wuRefresh() // the pending list changed
	}
	return code, out
}

func demoSetup(it catalog.Setup, args []string, onLine func(string)) (int, string) {
	lines := []string{"  (demo) " + it.Name}
	if it.Kind == "update" {
		lines = append(lines, "Searching Windows Update...", "  - 2026-09 Cumulative Update for Windows 11 (KB5066835)", "Installing...", "  installed 2026-09 Cumulative Update for Windows 11 (KB5066835)", "RESTART-NEEDED")
	}
	var out []string
	for _, l := range lines {
		time.Sleep(400 * time.Millisecond)
		if onLine != nil {
			onLine(l)
		}
		out = append(out, l)
	}
	return 0, strings.Join(out, "\n")
}

// setupPick is what a UI sends: an action id and, for "install specific updates", KB numbers.
type setupPick struct {
	ID   string   `json:"id"`
	Args []string `json:"args"`
}

func setupSteps(picks []setupPick) ([]Step, error) {
	var out []Step
	for _, p := range picks {
		it, ok := catalog.FindSetup(p.ID)
		if !ok {
			return nil, fmt.Errorf("unknown setup action: %s", p.ID)
		}
		var args []string
		for _, a := range p.Args {
			a = strings.ToUpper(strings.TrimSpace(a))
			if !strings.HasPrefix(a, "KB") {
				a = "KB" + a
			}
			if !catalog.SafeKB.MatchString(a) {
				return nil, fmt.Errorf("%q is not a KB number like KB5066835", a)
			}
			args = append(args, a)
		}
		if it.Arg == "kb" && len(args) == 0 {
			return nil, fmt.Errorf("%s needs at least one KB number", it.Name)
		}
		name := it.Name
		if len(args) > 0 {
			name += " (" + strings.Join(args, ", ") + ")"
		}
		out = append(out, Step{Type: "setup", Key: it.ID, App: name, Args: args})
	}
	return out, nil
}

// setupView is the catalog without the scripts, for UIs.
func setupView() []map[string]any {
	out := []map[string]any{}
	for _, it := range catalog.SetupItems() {
		out = append(out, map[string]any{"id": it.ID, "kind": it.Kind, "group": it.Group, "name": it.Name, "desc": it.Desc,
			"reboot": it.Reboot, "admin": it.Admin, "recommended": it.Recommended, "arg": it.Arg})
	}
	return out
}

// ---------------------------------------------------------------- pending Windows updates, refreshed in the background

type WinUpdate struct {
	Title    string `json:"title"`
	KB       string `json:"kb"`
	Security bool   `json:"security"`
	SizeMB   int64  `json:"size_mb"`
}

var (
	wuMu   sync.Mutex
	wuList []WinUpdate
	wuAt   time.Time
	wuBusy bool
	wuOnce sync.Once
	// wuChanged is called after each finished scan (the local UI uses it to refresh open pages)
	wuChanged func()
)

const psWUScan = `$ProgressPreference='SilentlyContinue';$s=(New-Object -ComObject Microsoft.Update.Session).CreateUpdateSearcher();` +
	`$r=$s.Search("IsInstalled=0 and IsHidden=0 and Type='Software'");` +
	`@($r.Updates|ForEach-Object{$c=@($_.Categories|ForEach-Object{$_.Name});[ordered]@{title=$_.Title;kb=(@($_.KBArticleIDs|ForEach-Object{"KB$_"}) -join ',');` +
	`security=(($c -contains 'Security Updates') -or ($c -contains 'Critical Updates'));size_mb=[int64]($_.MaxDownloadSize/1MB)}})|ForEach-Object -Begin{$a=@()} -Process{$a+=$_} -End{ConvertTo-Json -InputObject $a -Compress}`

// wuRefresh asks Windows Update what is pending. It can take a minute, so it never runs on a request path.
func wuRefresh() {
	if runtime.GOOS != "windows" && os.Getenv("OPENITE_DEMO") == "" {
		return
	}
	wuMu.Lock()
	if wuBusy {
		wuMu.Unlock()
		return
	}
	wuBusy = true
	wuMu.Unlock()
	var list []WinUpdate
	ok := true
	if os.Getenv("OPENITE_DEMO") != "" {
		list = []WinUpdate{{"2026-09 Cumulative Update for Windows 11 Version 25H2 for x64-based Systems (KB5066835)", "KB5066835", true, 712},
			{"Windows Malicious Software Removal Tool x64 - v5.140 (KB890830)", "KB890830", false, 78}}
	} else {
		rc, out := probe([]string{"powershell", "-NoProfile", "-NonInteractive", "-Command", psWUScan}, nil, 5*time.Minute)
		if rc != 0 || json.Unmarshal([]byte(strings.TrimSpace(out)), &list) != nil {
			ok = false
		}
	}
	wuMu.Lock()
	if ok {
		wuList, wuAt = list, time.Now()
	}
	wuBusy = false
	cb := wuChanged
	wuMu.Unlock()
	if ok && cb != nil {
		cb()
	}
}

// withWindowsUpdates adds the last known pending-update list to s and keeps it fresh (every 6 hours) in the background.
func withWindowsUpdates(s SysInfo) SysInfo {
	if runtime.GOOS != "windows" && os.Getenv("OPENITE_DEMO") == "" {
		return s
	}
	wuOnce.Do(func() {
		go func() {
			for {
				wuRefresh()
				time.Sleep(6 * time.Hour)
			}
		}()
	})
	wuMu.Lock()
	defer wuMu.Unlock()
	if !wuAt.IsZero() {
		s.WinUpdates, s.WUScanned = append([]WinUpdate{}, wuList...), wuAt.Unix()
	}
	return s
}
