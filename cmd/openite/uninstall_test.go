package main

import (
	"strings"
	"testing"
)

func TestSilentUninstallCmd(t *testing.T) {
	cases := []struct {
		name  string
		e     arpEntry
		want  string // substring expected in the command
		known bool
	}{
		{"quiet string wins", arpEntry{UninstallString: `"C:\a\u.exe"`, QuietUninstallString: `"C:\a\u.exe" /quiet`}, `/quiet`, true},
		{"msi", arpEntry{UninstallString: `MsiExec.exe /I{12345678-1234-1234-1234-123456789ABC}`}, `msiexec.exe /x {12345678-1234-1234-1234-123456789ABC} /qn /norestart`, true},
		{"inno", arpEntry{UninstallString: `"C:\Program Files\App\unins000.exe"`}, `/VERYSILENT /SUPPRESSMSGBOXES /NORESTART`, true},
		{"nsis (qBittorrent)", arpEntry{UninstallString: `"C:\Program Files\qBittorrent\uninst.exe"`}, `/S _?=C:\Program Files\qBittorrent`, true},
		{"unknown installer", arpEntry{UninstallString: `"C:\x\weird.exe" --remove`}, ``, false},
		{"empty", arpEntry{}, ``, false},
	}
	for _, c := range cases {
		got, _, ok := silentUninstallCmd(c.e)
		if ok != c.known || (ok && !strings.Contains(got, c.want)) {
			t.Errorf("%s: got (%q, %v), want contains %q known=%v", c.name, got, ok, c.want, c.known)
		}
	}
}

func TestExePathOf(t *testing.T) {
	if exe, rest := exePathOf(`"C:\Program Files\A B\u.exe" /x`); exe != `C:\Program Files\A B\u.exe` || rest != "/x" {
		t.Errorf("quoted: %q %q", exe, rest)
	}
	if exe, _ := exePathOf(`C:\Apps\u.exe /x`); exe != `C:\Apps\u.exe` {
		t.Errorf("unquoted: %q", exe)
	}
}
