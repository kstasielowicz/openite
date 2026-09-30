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
  openite pick                    choose presets and apps with the arrow keys, then install
  openite install firefox vlc     install apps by name
  openite install --preset developer
  openite update [app ...]        update apps (no names = update everything)
  openite install git@2.44.0      install an exact version (winget, apt)
  openite hold git | unhold git   stop / resume updates for an app (update-all skips held apps)
  openite uninstall app ...       silent uninstall, no windows to click through
  openite list [category]         show the catalog
  openite search <text>
  openite presets                 show starter packs
  openite status                  what is installed / outdated on this PC
  openite drivers                 detect your hardware and suggest the right driver tools
  openite schedule daily 03:00    update everything automatically (also: weekly mon 03:00, off, status)

Flags for install/update/uninstall:  -y (don't ask)  --dry-run (show, don't do)  --preset NAME

Managing many devices (needs an Openite server, see docs/SERVER.md)
  openite enroll --server URL --code CODE
  openite run | once | install-service | uninstall-service

Safety: openite only runs your OS package manager (winget, brew, apt) with package ids from its
built-in catalog. It never downloads or runs scripts from the internet.
`

func sortedApps() []catalog.App {
	apps := catalog.Apps()
	sort.SliceStable(apps, func(i, j int) bool {
		if apps[i].Category != apps[j].Category {
			return categoryRank(apps[i].Category) < categoryRank(apps[j].Category) ||
				(categoryRank(apps[i].Category) == categoryRank(apps[j].Category) && apps[i].Category < apps[j].Category)
		}
		return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name)
	})
	return apps
}

// Everyday categories first, plumbing (runtimes, drivers) last.
func categoryRank(c string) int {
	switch c {
	case "Runtimes & prerequisites":
		return 2
	case "Drivers & hardware":
		return 3
	}
	return 1
}

// resolve turns user input ("firefox", "Visual Studio Code", "vsc", "git@2.44.0", "preset:gaming") into catalog
// apps, plus any exact versions the user asked for.
func resolve(tokens []string, presets []string) ([]catalog.App, map[string]string, error) {
	all := catalog.Apps()
	var out []catalog.App
	versions := map[string]string{}
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
		ver := ""
		if i := strings.LastIndex(t, "@"); i > 0 {
			ver, t = t[i+1:], t[:i]
			if !catalog.SafeVersion.MatchString(ver) {
				return nil, nil, fmt.Errorf("%q is not a valid version", ver)
			}
		}
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
				return nil, nil, fmt.Errorf("unknown preset %q (see: openite presets)", id)
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
		var hit *catalog.App
		switch {
		case len(exact) == 1:
			hit = &exact[0]
		case len(exact) == 0 && len(partial) == 1:
			hit = &partial[0]
		case len(exact) == 0 && len(partial) == 0:
			return nil, nil, fmt.Errorf("no app matches %q (try: openite search %s)", t, t)
		default:
			names := []string{}
			for _, a := range append(exact, partial...) {
				names = append(names, a.Key)
			}
			return nil, nil, fmt.Errorf("%q is ambiguous: %s", t, strings.Join(names, ", "))
		}
		add(*hit)
		if ver != "" {
			versions[hit.Key] = ver
		}
	}
	return out, versions, nil
}

func cmdList(args []string) {
	filter := strings.ToLower(strings.Join(args, " "))
	banner("app catalog")
	last := ""
	for _, a := range sortedApps() {
		if filter != "" && !strings.Contains(strings.ToLower(a.Category), filter) {
			continue
		}
		if a.Category != last {
			fmt.Printf("\n  %s\n", bold(a.Category))
			last = a.Category
		}
		fmt.Printf("    %-22s %s\n", cyan(a.Key), a.Name)
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
			fmt.Printf("  %-22s %s  %s\n", cyan(a.Key), a.Name, dim("("+a.Category+")"))
			n++
		}
	}
	if n == 0 {
		fmt.Println("No matches. `openite list` shows everything.")
	}
}

func cmdPresets([]string) {
	banner("starter presets")
	for _, p := range catalog.Packs() {
		names := []string{}
		for _, k := range p.Apps {
			if a, ok := catalog.Find(k); ok {
				names = append(names, a.Name)
			}
		}
		fmt.Printf("\n  %s %s  %s\n      %s\n", p.Icon, bold(p.Name), dim("("+p.ID+")"), dim(strings.Join(names, ", ")))
	}
	fmt.Printf("\n  Install one with:  %s\n\n", cyan("openite install --preset developer"))
}

func needBackend() Backend {
	b := pickBackend()
	if b == nil {
		die("No supported package manager found. Openite needs winget (Windows 10/11), Homebrew (macOS) or apt (Debian/Ubuntu).")
	}
	return b
}

func inventoryWithSpinner(b Backend) catalog.Inventory {
	var inv catalog.Inventory
	withSpinner("Checking what's installed on this PC", func() bool { inv = b.Inventory(nil); return true })
	return inv
}

func cmdStatus([]string) {
	b := needBackend()
	banner("status of this PC")
	inv := inventoryWithSpinner(b)
	last := ""
	nInst, nOld := 0, 0
	for _, a := range sortedApps() {
		if a.Pkg(b.Name()) == "" {
			continue
		}
		in, old := a.State(b.Name(), inv)
		if !in {
			continue
		}
		if a.Category != last {
			fmt.Printf("\n  %s\n", bold(a.Category))
			last = a.Category
		}
		mark := okMark() + " " + a.Name
		if old {
			mark, nOld = yellow("↑")+" "+a.Name+"  "+yellow("update available"), nOld+1
		}
		nInst++
		fmt.Printf("    %s\n", mark)
	}
	fmt.Printf("\n  %s catalog apps installed, %s with updates.", green(strconv.Itoa(nInst)), yellow(strconv.Itoa(nOld)))
	if nOld > 0 {
		fmt.Printf("  Update all: %s", cyan("openite update"))
	}
	fmt.Print("\n\n")
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
	fmt.Print(q + " " + dim("[Y/n]") + " ")
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
	var versions map[string]string
	if !(kind == "upgrade" && len(o.names) == 0 && len(o.presets) == 0) {
		if len(o.names) == 0 && len(o.presets) == 0 {
			die("Tell me what to %s, e.g.  openite %s firefox vlc   or   openite %s --preset essentials\nOr run `openite pick` to choose interactively.", kind, os.Args[1], os.Args[1])
		}
		var err error
		if apps, versions, err = resolve(o.names, o.presets); err != nil {
			die("%v", err)
		}
	}
	runChange(kind, apps, versions, needBackend(), o.yes)
}

func stepFor(kind string, a catalog.App, pkg, version string) Step {
	return Step{Type: kind, App: a.Name, Pkg: pkg, Version: version, Aliases: append([]string{a.Name}, a.Match...)}
}

// runChange previews, asks, then runs each app as its own step so one failure doesn't stop the rest.
func runChange(kind string, apps []catalog.App, versions map[string]string, b Backend, yes bool) {
	verb := map[string]string{"install": "Install", "upgrade": "Update", "uninstall": "Uninstall", "pin": "Hold", "unpin": "Release hold on"}[kind]
	ing := map[string]string{"install": "Installing", "upgrade": "Updating", "uninstall": "Removing", "pin": "Holding", "unpin": "Releasing"}[kind]
	banner(strings.ToLower(verb))
	var steps []Step
	if kind == "upgrade" && len(apps) == 0 {
		fmt.Printf("\n  Will update %s that has an update available.\n", bold("everything"))
		steps = []Step{{Type: "upgrade", All: true}}
	} else {
		fmt.Println()
		for _, a := range apps {
			if pkg := a.Pkg(b.Name()); pkg != "" {
				if v := versions[a.Key]; v != "" {
					fmt.Printf("  %s %s %s\n", cyan("•"), a.Name, dim("version "+v))
				} else {
					fmt.Printf("  %s %s\n", cyan("•"), a.Name)
				}
				steps = append(steps, stepFor(kind, a, pkg, versions[a.Key]))
			} else {
				fmt.Printf("  %s %s %s\n", dim("•"), dim(a.Name), dim("(skipped: not available via "+b.Name()+")"))
			}
		}
		if len(steps) == 0 {
			die("Nothing to do.")
		}
	}
	if dryRun {
		fmt.Println("\n  " + yellow("Dry run: nothing will actually be changed."))
	}
	fmt.Println()
	if !yes && !confirm("  Proceed?") {
		fmt.Println("  Cancelled.")
		return
	}
	if kind == "uninstall" && !yes && !confirm("  Really uninstall? This removes the apps.") {
		return
	}
	fmt.Println()
	fail := 0
	for _, s := range steps {
		label := ing + " " + s.App
		if s.All {
			label = "Updating everything"
		}
		var lines []string
		ok := withSpinner(label, func() bool {
			return executeJob(Job{Steps: []Step{s}}, b, func(l string) { lines = append(lines, l) }) == "done"
		})
		if !ok {
			fail++
			fmt.Println(dim(indent(strings.Join(lines, "\n"), "      ")))
		} else if dryRun {
			for _, l := range lines {
				if strings.HasPrefix(l, "[dry-run]") {
					fmt.Println(dim("      " + l))
				}
			}
		}
	}
	fmt.Println()
	if fail > 0 {
		fmt.Printf("  %s %d of %d failed. Some installs need Administrator rights; try an elevated terminal.\n\n", failMark(), fail, len(steps))
		os.Exit(1)
	}
	fmt.Printf("  %s %s\n\n", okMark(), bold("All done."))
}

func indent(s, p string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	return p + strings.Join(lines, "\n"+p)
}

// cmdPick: presets first, then fine-tune individual apps, then confirm and install.
func cmdPick(args []string) {
	o := parseChangeArgs(args)
	if o.dry {
		dryRun = true
	}
	b := needBackend()
	if !interactive {
		pickPlain(b)
		return
	}
	banner("guided setup")
	fmt.Println()
	inv := inventoryWithSpinner(b)

	packs := catalog.Packs()
	var pitems []tuiItem
	for _, p := range packs {
		names := []string{}
		for _, k := range p.Apps {
			if a, ok := catalog.Find(k); ok && len(names) < 4 {
				names = append(names, a.Name)
			}
		}
		pitems = append(pitems, tuiItem{Key: p.ID, Label: p.Icon + " " + p.Name, Note: strings.Join(names, ", ") + "…"})
	}
	chosenPacks, ok := multiSelect("What do you want to set up?", "Pick any starter presets. You can fine-tune the exact apps on the next screen.", pitems)
	if !ok {
		fmt.Println("  Cancelled.")
		return
	}
	preselect := map[string]bool{}
	for _, id := range chosenPacks {
		for _, p := range packs {
			if p.ID == id {
				for _, k := range p.Apps {
					preselect[k] = true
				}
			}
		}
	}

	var aitems []tuiItem
	for _, a := range sortedApps() {
		if a.Pkg(b.Name()) == "" {
			continue
		}
		in, old := a.State(b.Name(), inv)
		it := tuiItem{Key: a.Key, Label: a.Name, Group: a.Category, Checked: preselect[a.Key] && !in, Dim: in}
		if old {
			it.Note, it.Dim = "update available", false
		} else if in {
			it.Note = "✔ installed"
		}
		aitems = append(aitems, it)
	}
	keys, ok := multiSelect("Choose your apps", "Space ticks an app. Already-installed apps are marked. Runtimes and drivers are at the bottom.", aitems)
	if !ok || len(keys) == 0 {
		fmt.Println("  Nothing selected.")
		return
	}
	var sel []catalog.App
	for _, k := range keys {
		a, _ := catalog.Find(k)
		sel = append(sel, a)
	}
	runChange("install", sel, nil, b, false)
}

// pickPlain is the no-terminal fallback (scripts, pipes): numbered lists.
func pickPlain(b Backend) {
	ask := func(q string) string {
		fmt.Print(q)
		l, err := stdin.ReadString('\n')
		if err != nil && l == "" {
			os.Exit(0)
		}
		return strings.TrimSpace(l)
	}
	fmt.Println("Openite: choose what to install.")
	inv := b.Inventory(nil)
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
	sep := func(r rune) bool { return r == ' ' || r == ',' }
	for _, f := range strings.FieldsFunc(ask("\nPreset numbers (e.g. 1 3), or Enter to skip: "), sep) {
		if n, err := strconv.Atoi(f); err == nil && n >= 1 && n <= len(packs) {
			for _, k := range packs[n-1].Apps {
				pick(k)
			}
		}
	}
	apps := sortedApps()
	fmt.Println("\nAll apps ([✔] = installed):")
	last := ""
	for i, a := range apps {
		if a.Category != last {
			fmt.Printf("\n  %s\n", a.Category)
			last = a.Category
		}
		mark, sel := " ", " "
		if installed, _ := a.State(b.Name(), inv); installed {
			mark = "✔"
		}
		if chosen[a.Key] {
			sel = "*"
		}
		fmt.Printf("   %s[%s] %3d  %s\n", sel, mark, i+1, a.Name)
	}
	for _, f := range strings.FieldsFunc(ask("\nAdd more: numbers or names (Enter for none): "), sep) {
		if n, err := strconv.Atoi(f); err == nil && n >= 1 && n <= len(apps) {
			pick(apps[n-1].Key)
		} else if r, _, err := resolve([]string{f}, nil); err == nil {
			pick(r[0].Key)
		}
	}
	var sel []catalog.App
	for _, k := range order {
		a, _ := catalog.Find(k)
		if installed, _ := a.State(b.Name(), inv); !installed {
			sel = append(sel, a)
		}
	}
	if len(sel) == 0 {
		fmt.Println("Nothing to install.")
		return
	}
	runChange("install", sel, nil, b, false)
}
