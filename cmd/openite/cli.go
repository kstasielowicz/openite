package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/openite/openite/catalog"
)

const usage = `openite: install and update software from one place.

Everyday use
  openite                         open the web UI for this PC
  openite pick                    choose presets/apps in the terminal, then install
  openite install firefox vlc     install apps by name
  openite install --preset developer
  openite update [app ...]        update apps (no names = update everything)
  openite uninstall app ...
  openite list [category]         show the catalog
  openite search <text>
  openite presets                 show starter packs
  openite status                  what is installed / outdated on this PC

Flags for install/update/uninstall:  -y (don't ask)  --dry-run (show, don't do)  --preset NAME

Managing many devices (needs an Openite server)
  openite enroll --server URL --code CODE
  openite run | once | install-service | uninstall-service

Safety: openite only runs your OS package manager (winget, brew, apt) with package ids from its
built-in catalog. It never downloads or runs scripts from the internet.
`

func sortedApps() []catalog.App {
	apps := catalog.Apps()
	sort.SliceStable(apps, func(i, j int) bool {
		if apps[i].Category != apps[j].Category {
			return apps[i].Category < apps[j].Category
		}
		return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name)
	})
	return apps
}

// resolve turns user input ("firefox", "Visual Studio Code", "vsc", "preset:gaming") into catalog apps.
func resolve(tokens []string, presets []string) ([]catalog.App, error) {
	all := catalog.Apps()
	var out []catalog.App
	seen := map[string]bool{}
	add := func(a catalog.App) {
		if !seen[a.Key] {
			seen[a.Key] = true
			out = append(out, a)
		}
	}
	for _, p := range presets {
		tokens = append(tokens, "preset:"+p)
	}
	for _, t := range tokens {
		low := strings.ToLower(strings.TrimSpace(t))
		if strings.HasPrefix(low, "preset:") {
			id := strings.TrimPrefix(low, "preset:")
			found := false
			for _, p := range catalog.Packs() {
				if strings.ToLower(p.ID) == id || strings.ToLower(p.Name) == id {
					found = true
					for _, k := range p.Apps {
						if a, ok := catalog.Find(k); ok {
							add(a)
						}
					}
				}
			}
			if !found {
				return nil, fmt.Errorf("unknown preset %q (see: openite presets)", id)
			}
			continue
		}
		var exact, partial []catalog.App
		for _, a := range all {
			n := strings.ToLower(a.Name)
			if a.Key == low || n == low {
				exact = append(exact, a)
			} else if strings.Contains(a.Key, low) || strings.Contains(n, low) {
				partial = append(partial, a)
			}
		}
		switch {
		case len(exact) == 1:
			add(exact[0])
		case len(exact) == 0 && len(partial) == 1:
			add(partial[0])
		case len(exact) == 0 && len(partial) == 0:
			return nil, fmt.Errorf("no app matches %q (try: openite search %s)", t, t)
		default:
			names := []string{}
			for _, a := range append(exact, partial...) {
				names = append(names, a.Key)
			}
			return nil, fmt.Errorf("%q is ambiguous: %s", t, strings.Join(names, ", "))
		}
	}
	return out, nil
}

func cmdList(args []string) {
	filter := strings.ToLower(strings.Join(args, " "))
	last := ""
	for _, a := range sortedApps() {
		if filter != "" && !strings.Contains(strings.ToLower(a.Category), filter) {
			continue
		}
		if a.Category != last {
			fmt.Printf("\n%s\n", a.Category)
			last = a.Category
		}
		fmt.Printf("  %-18s %s\n", a.Key, a.Name)
	}
	fmt.Println()
}

func cmdSearch(args []string) {
	q := strings.ToLower(strings.Join(args, " "))
	if q == "" {
		die("usage: openite search <text>")
	}
	n := 0
	for _, a := range sortedApps() {
		if strings.Contains(strings.ToLower(a.Name+" "+a.Key+" "+a.Category), q) {
			fmt.Printf("  %-18s %s  (%s)\n", a.Key, a.Name, a.Category)
			n++
		}
	}
	if n == 0 {
		fmt.Println("No matches. `openite list` shows everything.")
	}
}

func cmdPresets([]string) {
	for _, p := range catalog.Packs() {
		names := []string{}
		for _, k := range p.Apps {
			if a, ok := catalog.Find(k); ok {
				names = append(names, a.Name)
			}
		}
		fmt.Printf("  %-12s %s  %s\n               %s\n", p.ID, p.Icon, p.Name, strings.Join(names, ", "))
	}
	fmt.Println("\nInstall one with:  openite install --preset developer")
}

func needBackend() Backend {
	b := pickBackend()
	if b == nil {
		die("No supported package manager found. Openite needs winget (Windows 10/11), Homebrew (macOS) or apt (Debian/Ubuntu).")
	}
	return b
}

func stateOf(a catalog.App, b Backend, inv catalog.Inventory) (installed, outdated bool) {
	return a.State(b.Name(), inv)
}

func cmdStatus([]string) {
	b := needBackend()
	fmt.Println("Checking what's installed (this takes a few seconds)…")
	inv := b.Inventory()
	last := ""
	nInst, nOld := 0, 0
	for _, a := range sortedApps() {
		if a.Pkg(b.Name()) == "" {
			continue
		}
		in, old := stateOf(a, b, inv)
		if !in {
			continue
		}
		if a.Category != last {
			fmt.Printf("\n%s\n", a.Category)
			last = a.Category
		}
		mark := "✔"
		if old {
			mark, nOld = "↑ update available", nOld+1
		}
		nInst++
		fmt.Printf("  %-18s %s %s\n", a.Key, a.Name, mark)
	}
	fmt.Printf("\n%d catalog apps installed, %d with updates. Update all: openite update\n", nInst, nOld)
}

type changeOpts struct {
	yes, dry bool
	presets  []string
	names    []string
}

func parseChangeArgs(args []string) changeOpts {
	var o changeOpts
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-y" || a == "--yes":
			o.yes = true
		case a == "--dry-run":
			o.dry = true
		case a == "--preset" || a == "-p":
			if i+1 >= len(args) {
				die("--preset needs a name (see: openite presets)")
			}
			i++
			o.presets = append(o.presets, strings.Split(args[i], ",")...)
		case strings.HasPrefix(a, "--preset="):
			o.presets = append(o.presets, strings.Split(strings.TrimPrefix(a, "--preset="), ",")...)
		case strings.HasPrefix(a, "-"):
			die("unknown flag %s", a)
		default:
			o.names = append(o.names, strings.Split(a, ",")...)
		}
	}
	return o
}

var stdin = bufio.NewReader(os.Stdin) // one shared reader so prompts never lose buffered input

func confirm(q string) bool {
	fmt.Print(q + " [Y/n] ")
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" { // no terminal / closed pipe: never assume consent
		fmt.Println("\nNo input available; cancelled. Use -y to skip the question.")
		return false
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "" || line == "y" || line == "yes"
}

func cmdChange(kind string, args []string) {
	o := parseChangeArgs(args)
	if o.dry {
		dryRun = true
	}
	var apps []catalog.App
	if kind == "upgrade" && len(o.names) == 0 && len(o.presets) == 0 {
	} else {
		if len(o.names) == 0 && len(o.presets) == 0 {
			die("Tell me what to %s, e.g.  openite %s firefox vlc   or   openite %s --preset essentials\nOr run `openite pick` to choose interactively.", kind, os.Args[1], os.Args[1])
		}
		var err error
		if apps, err = resolve(o.names, o.presets); err != nil {
			die("%v", err)
		}
	}
	runChange(kind, apps, needBackend(), o.yes)
}

// runChange previews, asks, then runs each app as its own step so one failure doesn't stop the rest.
func runChange(kind string, apps []catalog.App, b Backend, yes bool) {
	verb := map[string]string{"install": "Install", "upgrade": "Update", "uninstall": "Uninstall"}[kind]
	var steps []Step
	if kind == "upgrade" && len(apps) == 0 {
		fmt.Println("Will update everything that has an update available.")
		steps = []Step{{Type: "upgrade", All: true}}
	} else {
		fmt.Printf("%s:\n", verb)
		for _, a := range apps {
			if pkg := a.Pkg(b.Name()); pkg != "" {
				fmt.Printf("  • %s\n", a.Name)
				steps = append(steps, Step{Type: kind, App: a.Name, Pkg: pkg})
			} else {
				fmt.Printf("  • %s (skipped: not available via %s)\n", a.Name, b.Name())
			}
		}
		if len(steps) == 0 {
			die("Nothing to do.")
		}
	}
	if dryRun {
		fmt.Println("\n(dry run: nothing will actually be changed)")
	}
	if !yes && !confirm("\nProceed?") {
		fmt.Println("Cancelled.")
		return
	}
	if kind == "uninstall" && !yes && !confirm("Really uninstall? This removes the apps.") {
		return
	}
	fail := 0
	for i, s := range steps {
		label := s.App
		if s.All {
			label = "everything"
		}
		fmt.Printf("[%d/%d] %s %s … ", i+1, len(steps), strings.ToLower(verb), label)
		var lines []string
		status := executeJob(Job{Steps: []Step{s}}, b, func(l string) { lines = append(lines, l) })
		if status == "done" {
			fmt.Println("done")
		} else {
			fail++
			fmt.Println("FAILED")
			fmt.Println(indent(strings.Join(lines, "\n"), "      "))
		}
	}
	if fail > 0 {
		fmt.Printf("\n%d of %d failed. Administrator rights are needed for some installs; try an elevated terminal.\n", fail, len(steps))
		os.Exit(1)
	}
	fmt.Println("\nAll done.")
}

func indent(s, p string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return p + strings.Join(lines, "\n"+p)
}

// cmdPick is the friendly interactive flow: presets first, then individual apps.
func cmdPick(args []string) {
	o := parseChangeArgs(args)
	if o.dry {
		dryRun = true
	}
	b := needBackend()
	ask := func(q string) string {
		fmt.Print(q)
		l, err := stdin.ReadString('\n')
		if err != nil && l == "" {
			os.Exit(0)
		}
		return strings.TrimSpace(l)
	}
	fmt.Println("Openite: choose what to install.\nChecking what's already installed…")
	inv := b.Inventory()

	packs := catalog.Packs()
	fmt.Println("\nStarter presets:")
	for i, p := range packs {
		fmt.Printf("  %d) %s %s\n", i+1, p.Icon, p.Name)
	}
	chosen := map[string]bool{}
	var order []string
	pick := func(k string) {
		if !chosen[k] {
			chosen[k] = true
			order = append(order, k)
		}
	}
	for _, f := range strings.FieldsFunc(ask("\nPreset numbers (e.g. 1 3), or press Enter to skip: "), func(r rune) bool { return r == ' ' || r == ',' }) {
		if n, err := strconv.Atoi(f); err == nil && n >= 1 && n <= len(packs) {
			for _, k := range packs[n-1].Apps {
				pick(k)
			}
		} else {
			fmt.Printf("  (ignored %q)\n", f)
		}
	}

	apps := sortedApps()
	fmt.Println("\nAll apps ([✔] = already installed):")
	last := ""
	for i, a := range apps {
		if a.Category != last {
			fmt.Printf("\n  %s\n", a.Category)
			last = a.Category
		}
		mark := " "
		if in, _ := stateOf(a, b, inv); in {
			mark = "✔"
		}
		sel := " "
		if chosen[a.Key] {
			sel = "*"
		}
		fmt.Printf("   %s[%s] %3d  %s\n", sel, mark, i+1, a.Name)
	}
	fmt.Println("\n  (* = selected by your presets)")
	for _, f := range strings.FieldsFunc(ask("\nAdd more: numbers or names, e.g. 4 12 vivaldi (Enter for none): "), func(r rune) bool { return r == ' ' || r == ',' }) {
		if n, err := strconv.Atoi(f); err == nil && n >= 1 && n <= len(apps) {
			pick(apps[n-1].Key)
		} else if r, err := resolve([]string{f}, nil); err == nil {
			pick(r[0].Key)
		} else {
			fmt.Printf("  (ignored: %v)\n", err)
		}
	}
	var sel []catalog.App
	for _, k := range order {
		a, _ := catalog.Find(k)
		if in, _ := stateOf(a, b, inv); in {
			fmt.Printf("  %s is already installed, skipping.\n", a.Name)
			continue
		}
		sel = append(sel, a)
	}
	if len(sel) == 0 {
		fmt.Println("Nothing to install.")
		return
	}
	fmt.Println()
	runChange("install", sel, b, false)
}
