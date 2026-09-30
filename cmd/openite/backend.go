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
	Install(pkg string) [][]string
	Uninstall(pkg string) [][]string
	Upgrade(pkg string) [][]string // pkg == "" upgrades everything
	OK(code int) bool
	Env() []string
	Inventory() catalog.Inventory
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

// ---- winget ---------------------------------------------------------------

type winget struct{}

var wingetBase = []string{"--accept-source-agreements", "--disable-interactivity"}
var wingetPkg = []string{"--accept-package-agreements", "--silent"}

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func (winget) Available() bool { return have("winget") }
func (winget) Name() string    { return "winget" }
func (winget) Env() []string   { return nil }
func (winget) Install(p string) [][]string {
	return [][]string{cat([]string{"winget", "install", "--id", p, "-e"}, wingetPkg, wingetBase)}
}
func (winget) Uninstall(p string) [][]string {
	return [][]string{cat([]string{"winget", "uninstall", "--id", p, "-e", "--silent"}, wingetBase)}
}
func (winget) Upgrade(p string) [][]string {
	if p == "" {
		return [][]string{cat([]string{"winget", "upgrade", "--all"}, wingetPkg, wingetBase)}
	}
	return [][]string{cat([]string{"winget", "upgrade", "--id", p, "-e"}, wingetPkg, wingetBase)}
}

// OK: success, "no applicable update" and "already installed" all count as fine (idempotent).
func (winget) OK(code int) bool {
	c := int32(uint32(code))
	return c == 0 || c == -1978335189 || c == -1978335135
}

func (winget) Inventory() catalog.Inventory {
	inv := catalog.Inventory{Installed: map[string]string{}, Upgradable: map[string]string{}, Names: map[string]string{}, UpgradableNames: map[string]string{}}
	_, out := run(cat([]string{"winget", "list"}, wingetBase), nil, 5*time.Minute)
	for _, r := range parseTable(out) {
		if r["Id"] != "" {
			inv.Installed[r["Id"]] = r["Version"]
		}
		if r["Name"] != "" { // apps installed outside winget show up as "ARP\..." ids; the display name is what identifies them
			inv.Names[r["Name"]] = r["Version"]
		}
	}
	_, out = run(cat([]string{"winget", "upgrade"}, wingetBase), nil, 5*time.Minute)
	for _, r := range parseTable(out) {
		if r["Id"] != "" {
			inv.Upgradable[r["Id"]] = r["Available"]
		}
		if r["Name"] != "" {
			inv.UpgradableNames[r["Name"]] = r["Available"]
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

// ---- apt ------------------------------------------------------------------

type apt struct{}

func (apt) Available() bool { return have("apt-get") }
func (apt) Name() string    { return "apt" }
func (apt) Env() []string   { return []string{"DEBIAN_FRONTEND=noninteractive"} }
func (apt) sudo() []string {
	if os.Geteuid() == 0 {
		return nil
	}
	return []string{"sudo", "-n"}
}
func (a apt) Install(p string) [][]string {
	return [][]string{cat(a.sudo(), []string{"apt-get", "install", "-y", p})}
}
func (a apt) Uninstall(p string) [][]string {
	return [][]string{cat(a.sudo(), []string{"apt-get", "remove", "-y", p})}
}
func (a apt) Upgrade(p string) [][]string {
	if p != "" {
		return [][]string{cat(a.sudo(), []string{"apt-get", "install", "-y", "--only-upgrade", p})}
	}
	return [][]string{cat(a.sudo(), []string{"apt-get", "update"}), cat(a.sudo(), []string{"apt-get", "upgrade", "-y"})}
}
func (apt) OK(c int) bool { return c == 0 }
func (apt) Inventory() catalog.Inventory {
	_, out := run([]string{"dpkg-query", "-W", "-f", "${Package} ${Version}\n"}, nil, time.Minute)
	inst := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.SplitN(l, " ", 2); len(f) == 2 {
			inst[f[0]] = f[1]
		}
	}
	_, out = run([]string{"apt", "list", "--upgradable"}, nil, time.Minute)
	up := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 1 && strings.Contains(f[0], "/") {
			up[strings.Split(f[0], "/")[0]] = f[1]
		}
	}
	return catalog.Inventory{Installed: inst, Upgradable: up}
}

// ---- brew -----------------------------------------------------------------

type brew struct{}

func (brew) Available() bool               { return have("brew") }
func (brew) Name() string                  { return "brew" }
func (brew) Env() []string                 { return nil }
func (brew) Install(p string) [][]string   { return [][]string{{"brew", "install", p}} }
func (brew) Uninstall(p string) [][]string { return [][]string{{"brew", "uninstall", p}} }
func (brew) OK(c int) bool                 { return c == 0 }
func (brew) Upgrade(p string) [][]string {
	if p != "" {
		return [][]string{{"brew", "upgrade", p}}
	}
	return [][]string{{"brew", "update"}, {"brew", "upgrade"}}
}

var brewOutdated = regexp.MustCompile(`^(\S+) .*< (\S+)`)

func (brew) Inventory() catalog.Inventory {
	inst := map[string]string{}
	for _, extra := range [][]string{{}, {"--cask"}} {
		_, out := run(cat([]string{"brew", "list", "--versions"}, extra), nil, time.Minute)
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) >= 2 {
				inst[f[0]] = f[len(f)-1]
			}
		}
	}
	_, out := run([]string{"brew", "outdated", "--verbose"}, nil, time.Minute)
	up := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if m := brewOutdated.FindStringSubmatch(l); m != nil {
			up[m[1]] = m[2]
		}
	}
	return catalog.Inventory{Installed: inst, Upgradable: up}
}

// ---- dry run (tests/demos) -----------------------------------------------

type dryBackend struct{}

func (dryBackend) Name() string                  { return "winget" }
func (dryBackend) Env() []string                 { return nil }
func (dryBackend) OK(c int) bool                 { return c == 0 }
func (dryBackend) Install(p string) [][]string   { return [][]string{{"echo", "install", p}} }
func (dryBackend) Uninstall(p string) [][]string { return [][]string{{"echo", "uninstall", p}} }
func (dryBackend) Upgrade(p string) [][]string   { return [][]string{{"echo", "upgrade", p}} }
func (dryBackend) Inventory() catalog.Inventory  { return catalog.Inventory{} }
