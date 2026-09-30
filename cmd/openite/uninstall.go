package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/openite/openite/catalog"
)

// Headless uninstall. `winget uninstall --silent` only works for apps winget itself installed; apps installed
// by hand (Vivaldi, qBittorrent, ...) show up as "ARP\..." entries and winget then runs their uninstaller with
// no silent switch, which pops up a window. For those we read the registry uninstall entry and add the right
// silent flags for the installer family (MSI / Inno Setup / NSIS), or use the app's own QuietUninstallString.

const uninstallTimeout = 10 * time.Minute // a hidden dialog must not hang us for an hour

type arpEntry struct {
	DisplayName          string
	UninstallString      string
	QuietUninstallString string
}

var (
	reGUID = regexp.MustCompile(`\{[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}\}`)
	reInno = regexp.MustCompile(`(?i)unins\d{3}\.exe`)
	reNSIS = regexp.MustCompile(`(?i)(^|[\\/"])(uninst|uninstall|uninstaller)[^\\/"]*\.exe`)
)

// winDir is filepath.Dir for Windows paths. It must not depend on the OS we run on: registry paths always use
// backslashes, and filepath.Dir on Linux (where CI also runs the tests) would not split them.
func winDir(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i > 0 {
		return p[:i]
	}
	return "."
}

// exePathOf splits `"C:\Program Files\App\uninst.exe" /arg` into the exe path and the rest.
func exePathOf(cmd string) (exe, rest string) {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, `"`) {
		if i := strings.Index(cmd[1:], `"`); i >= 0 {
			return cmd[1 : i+1], strings.TrimSpace(cmd[i+2:])
		}
	}
	if i := strings.Index(strings.ToLower(cmd), ".exe"); i >= 0 {
		return cmd[:i+4], strings.TrimSpace(cmd[i+4:])
	}
	return cmd, ""
}

// silentUninstallCmd turns a registry uninstall entry into a command line that needs no clicks.
// nsisExe is set when the NSIS "run in place" trick was used (the caller then removes the leftover exe).
func silentUninstallCmd(e arpEntry) (cmdline string, nsisExe string, ok bool) {
	if q := strings.TrimSpace(e.QuietUninstallString); q != "" {
		return q, "", true
	}
	u := strings.TrimSpace(e.UninstallString)
	if u == "" {
		return "", "", false
	}
	switch {
	case strings.Contains(strings.ToLower(u), "msiexec"):
		if g := reGUID.FindString(u); g != "" {
			return "msiexec.exe /x " + g + " /qn /norestart", "", true
		}
	case reInno.MatchString(u):
		exe, _ := exePathOf(u)
		return fmt.Sprintf(`"%s" /VERYSILENT /SUPPRESSMSGBOXES /NORESTART`, exe), "", true
	case reNSIS.MatchString(u):
		// NSIS uninstallers copy themselves to %TEMP% and return at once, so we'd report "done" while it is still
		// running. "_?=<dir>" makes it run in place and wait (it must be the last argument, unquoted).
		exe, _ := exePathOf(u)
		return fmt.Sprintf(`"%s" /S _?=%s`, exe, winDir(exe)), exe, true
	}
	return "", "", false
}

// smartUninstall: winget first (if winget owns the package), otherwise the registry uninstaller, always headless.
// In dry-run it still inspects the machine (read-only) and reports exactly what it would run.
func smartUninstall(s Step, b Backend) (int, string) {
	var log []string
	if s.Pkg != "" {
		if rc, _ := probe(cat([]string{"winget", "list", "--id", s.Pkg, "-e"}, wingetBase), nil, 2*time.Minute); rc == 0 {
			argv := cat([]string{"winget", "uninstall", "--id", s.Pkg, "-e", "--silent"}, wingetBase)
			if dryRun {
				return 0, "[dry-run] winget manages this app; would run: " + strings.Join(argv, " ")
			}
			rc, out := probe(argv, nil, uninstallTimeout)
			if b.OK(rc) {
				return rc, out
			}
			log = append(log, fmt.Sprintf("winget uninstall failed (exit %d), trying the app's own uninstaller:", rc), tail(out, 600))
		}
	}
	for _, e := range listARP() {
		for _, al := range s.Aliases {
			if !catalog.NameMatches(e.DisplayName, al) {
				continue
			}
			cmdline, nsisExe, ok := silentUninstallCmd(e)
			if !ok {
				log = append(log, "found \""+e.DisplayName+"\" but no silent uninstall method is known for it; remove it from Settings > Apps")
				return 1, strings.Join(log, "\n")
			}
			if dryRun {
				return 0, fmt.Sprintf("[dry-run] installed outside winget (\"%s\"); would run silently: %s", e.DisplayName, cmdline)
			}
			log = append(log, "running silent uninstaller: "+cmdline)
			rc, out := runRawCmdline(cmdline, uninstallTimeout)
			if rc == 0 && nsisExe != "" { // tidy the in-place NSIS uninstaller left behind
				os.Remove(nsisExe)
				os.Remove(winDir(nsisExe))
			}
			return rc, strings.Join(append(log, out), "\n")
		}
	}
	log = append(log, "could not find it installed (nothing to uninstall)")
	return 1, strings.Join(log, "\n")
}
