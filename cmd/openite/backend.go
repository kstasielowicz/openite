package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/openite/openite/catalog"
)

// Backend wraps one package manager. Commands are returned as argv lists so the
// agent never goes through a shell.
type Backend interface {
	Name() string
	Install(pkg, version string) [][]string // version "" = latest
	Uninstall(pkg string) [][]string
	Upgrade(pkg, version string) [][]string // pkg == "" upgrades everything (held packages are skipped by the manager)
	Pin(pkg string) [][]string              // hold: bulk updates skip it
	Unpin(pkg string) [][]string
	SupportsVersions() bool
	OK(code int) bool
	Env() []string
	// Inventory reads what is installed. progress (may be nil) is told the stage name as work proceeds.
	Inventory(progress func(stage string)) catalog.Inventory
}

var dryRun = os.Getenv("OPENITE_DRY_RUN") != ""

// run executes argv and returns the exit code plus combined output.
func run(argv []string, env []string, timeout time.Duration) (int, string) {
	if dryRun {
		return 0, "[dry-run] " + strings.Join(argv, " ")
	}
	return probe(argv, env, timeout)
}

// probe always really executes: for read-only questions ("is this installed?") that must be answered even in dry-run.
func probe(argv []string, env []string, timeout time.Duration) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return 124, string(out) + fmt.Sprintf("\ntimed out after %s", timeout)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	if errors.Is(err, exec.ErrNotFound) {
		return 127, "command not found: " + argv[0]
	}
	return -1, string(out) + "\n" + err.Error()
}

func have(bin string) bool { _, err := exec.LookPath(bin); return err == nil }

func pickBackend() Backend {
	if os.Getenv("OPENITE_DEMO") != "" {
		return newDemoBackend()
	}
	if dryRun {
		return &dryBackend{}
	}
	for _, b := range []Backend{&winget{}, &brew{}, &apt{}} {
		if b.(interface{ Available() bool }).Available() {
			return b
		}
	}
	return nil
}

func stage(progress func(string), name string) {
	if progress != nil {
		progress(name)
	}
}

func newInventory() catalog.Inventory {
	return catalog.Inventory{Installed: map[string]string{}, Upgradable: map[string]string{}, Names: map[string]string{},
		UpgradableNames: map[string]string{}, Pinned: map[string]string{}, PinnedNames: map[string]string{}}
}

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ---- winget ---------------------------------------------------------------

type winget struct{}

var wingetBase = []string{"--accept-source-agreements", "--disable-interactivity"}
var wingetPkg = []string{"--accept-package-agreements", "--silent"}

func withVersion(v string) []string {
	if v == "" {
		return nil
	}
	return []string{"--version", v}
}

func (winget) Available() bool        { return have("winget") }
func (winget) Name() string           { return "winget" }
func (winget) Env() []string          { return nil }
func (winget) SupportsVersions() bool { return true }
func (winget) Install(p, v string) [][]string {
	return [][]string{cat([]string{"winget", "install", "--id", p, "-e"}, withVersion(v), wingetPkg, wingetBase)}
}
func (winget) Uninstall(p string) [][]string {
	return [][]string{cat([]string{"winget", "uninstall", "--id", p, "-e", "--silent"}, wingetBase)}
}
func (winget) Upgrade(p, v string) [][]string {
	if p == "" {
		return [][]string{cat([]string{"winget", "upgrade", "--all"}, wingetPkg, wingetBase)}
	}
	return [][]string{cat([]string{"winget", "upgrade", "--id", p, "-e"}, withVersion(v), wingetPkg, wingetBase)}
}
func (winget) Pin(p string) [][]string {
	return [][]string{cat([]string{"winget", "pin", "add", "--id", p, "-e"}, wingetBase)}
}
func (winget) Unpin(p string) [][]string {
	return [][]string{cat([]string{"winget", "pin", "remove", "--id", p, "-e"}, wingetBase)}
}

// OK: success, "no applicable update" and "already installed" all count as fine (idempotent).
func (winget) OK(code int) bool {
	c := int32(uint32(code))
	return c == 0 || c == -1978335189 || c == -1978335135
}

func (winget) Inventory(progress func(string)) catalog.Inventory {
	inv := newInventory()
	stage(progress, "Reading installed apps")
	_, out := probe(cat([]string{"winget", "list"}, wingetBase), nil, 5*time.Minute)
	for _, r := range parseTable(out) {
		if r["Id"] != "" {
			inv.Installed[r["Id"]] = r["Version"]
		}
		if r["Name"] != "" { // apps installed outside winget show up as "ARP\..." ids; the display name is what identifies them
			inv.Names[r["Name"]] = r["Version"]
		}
	}
	stage(progress, "Checking for updates")
	_, out = probe(cat([]string{"winget", "upgrade"}, wingetBase), nil, 5*time.Minute)
	for _, r := range parseTable(out) {
		if r["Id"] != "" {
			inv.Upgradable[r["Id"]] = r["Available"]
		}
		if r["Name"] != "" {
			inv.UpgradableNames[r["Name"]] = r["Available"]
		}
	}
	stage(progress, "Reading held packages")
	_, out = probe(cat([]string{"winget", "pin", "list"}, wingetBase), nil, time.Minute)
	for _, r := range parseTable(out) {
		if r["Id"] != "" {
			inv.Pinned[r["Id"]] = r["Version"]
		}
		if r["Name"] != "" {
			inv.PinnedNames[r["Name"]] = r["Version"]
		}
	}
	return inv
}

// parseTable parses `winget list/upgrade` output: a header line, a dashed line, then
// fixed-width rows. Columns are sliced by the header word positions (in runes, so
// wide/accented characters don't shift things).
func parseTable(text string) []map[string]string {
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	sep := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if i > 0 && len(t) > 5 && strings.Trim(t, "-") == "" {
			sep = i
			break
		}
	}
	if sep < 0 {
		return nil
	}
	type col struct {
		name string
		pos  int
	}
	var cols []col
	h := []rune(lines[sep-1])
	for i := 0; i < len(h); {
		if h[i] == ' ' {
			i++
			continue
		}
		j := i
		for j < len(h) && h[j] != ' ' {
			j++
		}
		cols = append(cols, col{string(h[i:j]), i})
		i = j
	}
	if len(cols) < 2 {
		return nil
	}
	var rows []map[string]string
	for _, l := range lines[sep+1:] {
		r := []rune(l)
		// Real rows have a space right before the Id column; blank lines and footers ("3 upgrades available.") don't.
		if len(r) <= cols[1].pos || r[cols[1].pos-1] != ' ' {
			continue
		}
		row := map[string]string{}
		for k, c := range cols {
			end := len(r)
			if k+1 < len(cols) && cols[k+1].pos < end {
				end = cols[k+1].pos
			}
			if c.pos < end {
				row[c.name] = strings.TrimSpace(string(r[c.pos:end]))
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// versionList parses `winget show --id X --versions`: a header, a dashed line, then one version per line (newest first).
func versionList(out string) []string {
	var vs []string
	past := false
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		t := strings.TrimSpace(l)
		if !past {
			if len(t) > 3 && strings.Trim(t, "-") == "" {
				past = true
			}
			continue
		}
		if t != "" && catalog.SafeVersion.MatchString(t) {
			vs = append(vs, t)
		}
	}
	return vs
}

// ---- apt ------------------------------------------------------------------

type apt struct{}

func (apt) Available() bool        { return have("apt-get") }
func (apt) Name() string           { return "apt" }
func (apt) Env() []string          { return []string{"DEBIAN_FRONTEND=noninteractive"} }
func (apt) SupportsVersions() bool { return true }
func (apt) sudo() []string {
	if os.Geteuid() == 0 {
		return nil
	}
	return []string{"sudo", "-n"}
}
func (a apt) Install(p, v string) [][]string {
	if v != "" {
		return [][]string{cat(a.sudo(), []string{"apt-get", "install", "-y", "--allow-downgrades", p + "=" + v})}
	}
	return [][]string{cat(a.sudo(), []string{"apt-get", "install", "-y", p})}
}
func (a apt) Uninstall(p string) [][]string {
	return [][]string{cat(a.sudo(), []string{"apt-get", "remove", "-y", p})}
}
func (a apt) Upgrade(p, v string) [][]string {
	if p != "" && v != "" {
		return a.Install(p, v)
	}
	if p != "" {
		return [][]string{cat(a.sudo(), []string{"apt-get", "install", "-y", "--only-upgrade", p})}
	}
	return [][]string{cat(a.sudo(), []string{"apt-get", "update"}), cat(a.sudo(), []string{"apt-get", "upgrade", "-y"})}
}
func (a apt) Pin(p string) [][]string {
	return [][]string{cat(a.sudo(), []string{"apt-mark", "hold", p})}
}
func (a apt) Unpin(p string) [][]string {
	return [][]string{cat(a.sudo(), []string{"apt-mark", "unhold", p})}
}
func (apt) OK(c int) bool { return c == 0 }
func (apt) Inventory(progress func(string)) catalog.Inventory {
	inv := newInventory()
	stage(progress, "Reading installed packages")
	_, out := probe([]string{"dpkg-query", "-W", "-f", "${Package} ${Version}\n"}, nil, time.Minute)
	for _, l := range strings.Split(out, "\n") {
		if f := strings.SplitN(l, " ", 2); len(f) == 2 {
			inv.Installed[f[0]] = f[1]
		}
	}
	stage(progress, "Checking for updates")
	_, out = probe([]string{"apt", "list", "--upgradable"}, nil, time.Minute)
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 1 && strings.Contains(f[0], "/") {
			inv.Upgradable[strings.Split(f[0], "/")[0]] = f[1]
		}
	}
	stage(progress, "Reading held packages")
	_, out = probe([]string{"apt-mark", "showhold"}, nil, time.Minute)
	for _, l := range strings.Fields(out) {
		inv.Pinned[l] = inv.Installed[l]
	}
	return inv
}

// ---- brew -----------------------------------------------------------------

type brew struct{}

func (brew) Available() bool                { return have("brew") }
func (brew) Name() string                   { return "brew" }
func (brew) Env() []string                  { return nil }
func (brew) SupportsVersions() bool         { return false } // Homebrew has no "install exactly this version"
func (brew) Install(p, _ string) [][]string { return [][]string{{"brew", "install", p}} }
func (brew) Uninstall(p string) [][]string  { return [][]string{{"brew", "uninstall", p}} }
func (brew) Pin(p string) [][]string        { return [][]string{{"brew", "pin", p}} }
func (brew) Unpin(p string) [][]string      { return [][]string{{"brew", "unpin", p}} }
func (brew) OK(c int) bool                  { return c == 0 }
func (brew) Upgrade(p, _ string) [][]string {
	if p != "" {
		return [][]string{{"brew", "upgrade", p}}
	}
	return [][]string{{"brew", "update"}, {"brew", "upgrade"}}
}

var brewOutdated = regexp.MustCompile(`^(\S+) .*< (\S+)`)

func (brew) Inventory(progress func(string)) catalog.Inventory {
	inv := newInventory()
	stage(progress, "Reading installed packages")
	for _, extra := range [][]string{{}, {"--cask"}} {
		_, out := probe(cat([]string{"brew", "list", "--versions"}, extra), nil, time.Minute)
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) >= 2 {
				inv.Installed[f[0]] = f[len(f)-1]
			}
		}
	}
	stage(progress, "Checking for updates")
	_, out := probe([]string{"brew", "outdated", "--verbose"}, nil, time.Minute)
	for _, l := range strings.Split(out, "\n") {
		if m := brewOutdated.FindStringSubmatch(l); m != nil {
			inv.Upgradable[m[1]] = m[2]
		}
	}
	stage(progress, "Reading held packages")
	_, out = probe([]string{"brew", "list", "--pinned"}, nil, time.Minute)
	for _, l := range strings.Fields(out) {
		inv.Pinned[l] = inv.Installed[l]
	}
	return inv
}

// ---- dry run (tests/demos) -----------------------------------------------

type dryBackend struct{}

func (dryBackend) Name() string           { return "winget" }
func (dryBackend) Env() []string          { return nil }
func (dryBackend) OK(c int) bool          { return c == 0 }
func (dryBackend) SupportsVersions() bool { return true }
func (dryBackend) Install(p, v string) [][]string {
	return [][]string{{"echo", "install", p, v}}
}
func (dryBackend) Uninstall(p string) [][]string { return [][]string{{"echo", "uninstall", p}} }
func (dryBackend) Upgrade(p, v string) [][]string {
	return [][]string{{"echo", "upgrade", p, v}}
}
func (dryBackend) Pin(p string) [][]string   { return [][]string{{"echo", "pin", p}} }
func (dryBackend) Unpin(p string) [][]string { return [][]string{{"echo", "unpin", p}} }
func (dryBackend) Inventory(func(string)) catalog.Inventory {
	return newInventory()
}
