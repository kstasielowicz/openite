# Command-line guide

```text
openite                      open the web UI for this PC
openite pick                 guided setup: presets → apps → confirm → install
openite install <app…>       e.g. openite install firefox vlc vivaldi
openite install --preset developer
openite update [app…]        no names = update everything
openite uninstall <app…>     silent, no windows to click through
openite list | search | presets | status
openite drivers              detect hardware, suggest the right driver tools
openite schedule daily 03:00 automatic updates on this PC
```
Flags for install / update / uninstall: `-y` (don't ask), `--dry-run` (show what would happen, change nothing), `--preset NAME`.

## Guided setup: `openite pick`
Arrow keys to move, **Space** to tick, **/** to search, **a** to tick a whole category, **Enter** to continue, **q** to quit.

1. Pick **presets** (Everyday essentials, Developer, Gaming, Creator, Home lab, Privacy & security, Runtimes for games & apps).
2. Fine-tune the exact apps. Installed apps are marked, and runtimes / drivers sit at the bottom.
3. Confirm, then watch each app install with a live spinner and ✔ / ✘.

Without a real terminal (scripts, pipes) it falls back to numbered lists.

## Names are forgiving
`openite install "visual studio code"`, `vscode`, `vsc` all work when unambiguous; otherwise it lists the candidates. Preset names are accepted as `--preset gaming` or `preset:gaming`.

## Runtimes and prerequisites
Category **Runtimes & prerequisites**: Visual C++ redistributables (2010 → 2015-2022, x86 and x64), .NET Desktop Runtime 8/9/10, .NET Runtime and SDK, ASP.NET Core, DirectX End-User Runtime, WebView2, Java (Temurin JRE 17/21). The preset **Runtimes for games & apps** installs the common set that fixes most "missing DLL" errors:
```powershell
openite install --preset runtimes
```
Installed runtimes are detected (by their Windows "installed programs" names), so they show as ✔ installed.

## Drivers
```powershell
openite drivers
```
Detects your PC maker/model, CPU and GPU and suggests the right **official** source:
- Installs the vendor's own update tool when winget carries it: **Intel Driver & Support Assistant**, **Dell Command | Update**, **Lenovo System Update**.
- Otherwise opens the official download page (NVIDIA, AMD, HP, ASUS, MSI, Gigabyte), since those vendors don't publish to winget.
- Always suggests **Windows Update** (audio, network, touchpad drivers).

Openite deliberately **does not** download or install raw driver packages itself: a wrong or tampered driver can make a PC unbootable, so the vendor's installer does that part. The same suggestions appear in the web UI under *Recommended for this PC*.

## Headless uninstall
`openite uninstall <app>` never needs a click:
1. If winget manages the app: `winget uninstall --silent`.
2. Otherwise (apps installed by hand, which winget lists as `ARP\…`): Openite reads the app's registry uninstall entry and runs it with the right silent switches for its installer type (MSI `/qn`, Inno Setup `/VERYSILENT`, NSIS `/S`, or the app's own `QuietUninstallString`).
3. Each step has a 10-minute limit, so a hidden dialog can't hang the run; the job is reported as failed with the reason.

`--dry-run` shows which path and command it would use, without uninstalling. If an app still opens a window, tell us which one (issue with its name) and we'll add a rule for it.

## Automatic updates on this PC
See [AUTO-UPDATES.md](AUTO-UPDATES.md): `openite schedule daily 03:00`.

## Install and update with one line
```powershell
irm https://raw.githubusercontent.com/kstasielowicz/openite/main/install.ps1 | iex
```
[`install.ps1`](../install.ps1) documents every step it performs; it verifies a SHA-256 checksum before installing and works without admin rights.
