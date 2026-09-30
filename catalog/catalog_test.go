package catalog

import "testing"

func TestNameMatches(t *testing.T) {
	yes := [][2]string{{"Vivaldi", "Vivaldi"}, {"Python 3.12.10 (64-bit)", "Python 3"}, {"7-Zip 26.03 (x64)", "7-Zip"},
		{"GIMP 3.0.4", "GIMP"}, {"Zoom Workplace (64-bit)", "Zoom Workplace"}, {"Git", "Git"}, {"Node.js", "Node.js"}}
	no := [][2]string{{"GitHub Desktop", "Git"}, {"Git Extensions", "Git"}, {"Python Launcher", "Python 3"}, {"Gopher", "Go"}, {"Visual Studio Code Helper", "Visual Studio Code"}}
	for _, c := range yes {
		if !NameMatches(c[0], c[1]) {
			t.Errorf("%q should match %q", c[0], c[1])
		}
	}
	for _, c := range no {
		if NameMatches(c[0], c[1]) {
			t.Errorf("%q must not match %q", c[0], c[1])
		}
	}
}

func TestVivaldiInstalledOutsideWinget(t *testing.T) {
	a, _ := Find("vivaldi")
	inv := Inventory{Installed: map[string]string{`ARP\User\X64\Vivaldi`: "8.2"}, Names: map[string]string{"Vivaldi": "8.2"}}
	if ok, _ := a.State("winget", inv); !ok {
		t.Fatal("Vivaldi (listed only by name) must be detected")
	}
}

func TestCatalogIsSane(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range Apps() {
		if seen[a.Key] {
			t.Errorf("duplicate key %s", a.Key)
		}
		seen[a.Key] = true
		for _, id := range []string{a.Winget, a.Brew, a.Apt} {
			if id != "" && !SafeID.MatchString(id) {
				t.Errorf("%s: unsafe package id %q", a.Key, id)
			}
		}
		if a.Winget == "" && a.Brew == "" && a.Apt == "" {
			t.Errorf("%s has no package id", a.Key)
		}
	}
	for _, p := range Packs() {
		for _, k := range p.Apps {
			if !seen[k] {
				t.Errorf("pack %s references unknown app %s", p.ID, k)
			}
		}
	}
}
