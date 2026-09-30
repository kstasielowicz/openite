package main

import (
	"testing"

	"github.com/openite/openite/catalog"
)

func TestCmpVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.9", "2.10", -1}, {"2.10.1", "2.10", 1}, {"1.0", "1.0.0", 0}, {"115.0.3", "115.0.3", 0}, {"v1.2", "1.3", -1},
	}
	for _, c := range cases {
		if got := cmpVersions(c.a, c.b); got != c.want {
			t.Errorf("cmp(%s,%s)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestVersionListParsing(t *testing.T) {
	out := "Found Git [Git.Git]\nVersion\n-------\n2.45.2\n2.45.1\n2.44.0\n"
	got := versionList(out)
	if len(got) != 3 || got[0] != "2.45.2" || got[2] != "2.44.0" {
		t.Errorf("got %v", got)
	}
}

func TestResolveVersionSyntax(t *testing.T) {
	apps, vers, err := resolve([]string{"git@2.44.0", "vlc"}, nil)
	if err != nil || len(apps) != 2 || vers["git"] != "2.44.0" || vers["vlc"] != "" {
		t.Fatalf("apps=%v vers=%v err=%v", apps, vers, err)
	}
	for _, bad := range []string{"git@--force", "git@1 2", "git@"} {
		if _, _, err := resolve([]string{bad}, nil); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestStatusVersionAndHeld(t *testing.T) {
	a, _ := catalog.Find("git")
	inv := catalog.Inventory{
		Installed: map[string]string{"Git.Git": "2.44.0"}, Upgradable: map[string]string{"Git.Git": "2.45.2"},
		Pinned: map[string]string{"Git.Git": ""},
	}
	st := a.Status("winget", inv)
	if !st.Installed || !st.Upgradable || !st.Held || st.Version != "2.44.0" || st.Available != "2.45.2" {
		t.Errorf("unexpected status %+v", st)
	}
	v, _ := catalog.Find("vivaldi")
	st = v.Status("winget", catalog.Inventory{Names: map[string]string{"Vivaldi": "8.2"}})
	if !st.Installed || st.Version != "8.2" {
		t.Errorf("name-matched app should carry its version: %+v", st)
	}
}

func TestVendorHintForVM(t *testing.T) {
	if vmFromModel("QEMU", "Standard PC (Q35)") == "" || vmFromModel("VMware, Inc.", "VMware7,1") == "" {
		t.Error("VMs not recognised")
	}
	if vmFromModel("Dell Inc.", "XPS 15") != "" {
		t.Error("physical machine flagged as VM")
	}
}
