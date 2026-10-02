// Package catalog embeds the app catalog and starter packs, and decides whether an app is
// present in a device's inventory. Keep the matching rules in sync with server/server.py.
package catalog

import (
	"embed"
	"encoding/json"
	"regexp"
	"strings"
)

//go:embed catalog.json packs.json
var files embed.FS

type App struct {
	Key      string   `json:"key"`
	Name     string   `json:"name"`
	Category string   `json:"category"`
	Winget   string   `json:"winget,omitempty"`
	Brew     string   `json:"brew,omitempty"`
	Apt      string   `json:"apt,omitempty"`
	Match    []string `json:"match,omitempty"` // extra display names seen in installed-program lists
}

type Pack struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Icon string   `json:"icon"`
	Apps []string `json:"apps"`
}

// Inventory is what a package manager reports for a device.
type Inventory struct {
	Installed       map[string]string // package id -> version
	Upgradable      map[string]string // package id -> available version
	Names           map[string]string // display name -> version (winget lists apps installed outside winget by name only)
	UpgradableNames map[string]string
	Pinned          map[string]string // held package id -> version (bulk updates skip these)
	PinnedNames     map[string]string
	Labels          map[string]string // package id -> display name, when the manager reports one
}

// SafeVersion is the only shape of version string we will pass to a package manager.
var SafeVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~:-]{0,63}$`)

// SafeID is the only shape of package id we will ever pass to a package manager.
var SafeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+@/-]{0,127}$`)

func mustLoad(name string, v any) {
	b, err := files.ReadFile(name)
	if err == nil {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		panic("catalog: " + name + ": " + err.Error())
	}
}

func Apps() []App   { var a []App; mustLoad("catalog.json", &a); return a }
func Packs() []Pack { var p []Pack; mustLoad("packs.json", &p); return p }

func Find(key string) (App, bool) {
	for _, a := range Apps() {
		if a.Key == key {
			return a, true
		}
	}
	return App{}, false
}

// Pkg returns the package id for a package manager ("winget", "brew", "apt"), or "".
func (a App) Pkg(manager string) string {
	switch manager {
	case "winget":
		return a.Winget
	case "brew":
		return a.Brew
	case "apt":
		return a.Apt
	}
	return ""
}

func norm(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// NameMatches reports whether an installed display name is this alias, optionally followed
// by a version or arch: "Python 3" matches "Python 3.12.10 (64-bit)", but "Git" does not
// match "GitHub Desktop" or "Git Extensions".
func NameMatches(have, alias string) bool {
	h, a := norm(have), norm(alias)
	if a == "" || !strings.HasPrefix(h, a) {
		return false
	}
	if len(h) == len(a) {
		return true
	}
	c := h[len(a)]
	if c != ' ' && c != '(' && c != '.' && !isDigit(c) {
		return false
	}
	rest := strings.TrimSpace(h[len(a):])
	if rest == "" {
		return true
	}
	r := rest[0]
	return isDigit(r) || r == '(' || r == '.' || r == '-' || (r == 'v' && len(rest) > 1 && isDigit(rest[1]))
}

func hasFold(m map[string]string, id string) bool {
	for k := range m {
		if strings.EqualFold(k, id) {
			return true
		}
	}
	return false
}

func anyName(m map[string]string, aliases []string) bool {
	for n := range m {
		for _, al := range aliases {
			if NameMatches(n, al) {
				return true
			}
		}
	}
	return false
}

// AppStatus is everything the UI needs to know about one app on one device.
type AppStatus struct {
	Installed  bool
	Upgradable bool
	Held       bool
	Version    string // installed version ("" if unknown)
	Available  string // newer version offered by the package manager ("" if none)
}

func lookupFold(m map[string]string, id string) (string, bool) {
	for k, v := range m {
		if strings.EqualFold(k, id) {
			return v, true
		}
	}
	return "", false
}

func lookupName(m map[string]string, aliases []string) (string, bool) {
	for n, v := range m {
		for _, al := range aliases {
			if NameMatches(n, al) {
				return v, true
			}
		}
	}
	return "", false
}

// Status looks the app up by package id first, then by the display name Windows lists it under.
func (a App) Status(manager string, inv Inventory) AppStatus {
	aliases := append([]string{a.Name}, a.Match...)
	var st AppStatus
	pkg := a.Pkg(manager)
	if pkg != "" {
		if v, ok := lookupFold(inv.Installed, pkg); ok {
			st.Installed, st.Version = true, v
		}
		if v, ok := lookupFold(inv.Upgradable, pkg); ok {
			st.Upgradable, st.Available = true, v
		}
		if _, ok := lookupFold(inv.Pinned, pkg); ok {
			st.Held = true
		}
	}
	if !st.Installed {
		if v, ok := lookupName(inv.Names, aliases); ok {
			st.Installed, st.Version = true, v
		}
	}
	if !st.Upgradable {
		if v, ok := lookupName(inv.UpgradableNames, aliases); ok {
			st.Upgradable, st.Available = true, v
		}
	}
	if !st.Held {
		if _, ok := lookupName(inv.PinnedNames, aliases); ok {
			st.Held = true
		}
	}
	st.Installed = st.Installed || st.Upgradable
	return st
}

// State says whether the app is installed and whether an update is available.
func (a App) State(manager string, inv Inventory) (installed, upgradable bool) {
	st := a.Status(manager, inv)
	return st.Installed, st.Upgradable
}
