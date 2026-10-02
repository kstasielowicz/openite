package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/openite/openite/catalog"
)

func TestSetupCatalog(t *testing.T) {
	seen := map[string]bool{}
	for _, it := range catalog.SetupItems() {
		if seen[it.ID] || it.Name == "" || len(it.PS) == 0 {
			t.Errorf("bad or duplicate setup item %q", it.ID)
		}
		seen[it.ID] = true
		if it.Kind != "tweak" && it.Kind != "feature" && it.Kind != "update" {
			t.Errorf("%s: unknown kind %q", it.ID, it.Kind)
		}
	}
	for _, kb := range []string{"KB5066835", "KB890830"} {
		if !catalog.SafeKB.MatchString(kb) {
			t.Errorf("%s should be accepted", kb)
		}
	}
	for _, kb := range []string{"KB1", "KB123456'; rm", "5066835", "kb5066835"} {
		if catalog.SafeKB.MatchString(kb) {
			t.Errorf("%q should be refused", kb)
		}
	}
	if code, _ := runSetup(Step{Key: "wu-kb", Args: []string{"KB5'; x"}}, nil); code == 0 {
		t.Error("unsafe KB was not refused")
	}
	if code, _ := runSetup(Step{Key: "wu-kb"}, nil); code == 0 {
		t.Error("KB action without KBs was not refused")
	}
}

// Every generated script must at least parse; a typo would otherwise only show up on a user's PC.
func TestSetupScriptsParse(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell parser check runs on Windows")
	}
	dir := t.TempDir()
	for _, it := range catalog.SetupItems() {
		f := filepath.Join(dir, it.ID+".ps1")
		os.WriteFile(f, []byte(setupScript(it, []string{"KB5066835"})), 0o600)
		ps := `$e=$null;[void][System.Management.Automation.Language.Parser]::ParseFile('` + f + `',[ref]$null,[ref]$e);if($e){$e|ForEach-Object{$_.Message};exit 1}`
		out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).CombinedOutput()
		if err != nil {
			t.Errorf("%s does not parse: %s", it.ID, strings.TrimSpace(string(out)))
		}
	}
}
