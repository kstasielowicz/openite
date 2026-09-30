package main

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

const psListARP = `$p='HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*','HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*','HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*';` +
	`Get-ItemProperty $p -ErrorAction SilentlyContinue | Where-Object { $_.DisplayName } | Select-Object DisplayName,UninstallString,QuietUninstallString | ConvertTo-Json -Compress`

func listARP() []arpEntry {
	_, out := probe([]string{"powershell", "-NoProfile", "-NonInteractive", "-Command", psListARP}, nil, 2*time.Minute)
	var many []arpEntry
	if json.Unmarshal([]byte(out), &many) != nil {
		var one arpEntry // PowerShell prints a bare object when there is a single result
		if json.Unmarshal([]byte(out), &one) == nil && one.DisplayName != "" {
			many = []arpEntry{one}
		}
	}
	return many
}

// runRawCmdline runs a registry-provided command line through cmd.exe exactly as Windows itself would.
func runRawCmdline(line string, timeout time.Duration) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /C "` + line + `"`, HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return 124, string(out) + "\ntimed out: the uninstaller probably opened a window that needs a click"
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	return -1, err.Error()
}
