package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openite/openite/catalog"
)

// Demo backend (OPENITE_DEMO=1): behaves like winget for the UI and the agent, but every "install" is a short-lived
// child process of this very binary that prints realistic, slowly arriving output. Nothing on the machine is touched.
// It exists so streaming, progress and failure hints can be developed and tested for real. Set OPENITE_DEMO_FAIL=<text>
// to make any package whose id contains <text> fail with an MSI-style error.

type demoBackend struct {
	mu        sync.Mutex
	installed map[string]string
	pinned    map[string]bool
}

func newDemoBackend() *demoBackend {
	return &demoBackend{
		installed: map[string]string{"Git.Git": "2.50.1", "Microsoft.VisualStudioCode": "1.99.0", "7zip.7zip": "24.08", "Python.Python.3.12": "3.12.10", "VideoLAN.VLC": "3.0.20", "Mozilla.Firefox": "128.0"},
		pinned:    map[string]bool{},
	}
}

func (d *demoBackend) Name() string           { return "winget" }
func (d *demoBackend) Env() []string          { return nil }
func (d *demoBackend) OK(c int) bool          { return c == 0 }
func (d *demoBackend) SupportsVersions() bool { return true }

func (d *demoBackend) step(verb, pkg, version string) [][]string {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	fail := "0"
	if f := os.Getenv("OPENITE_DEMO_FAIL"); f != "" && strings.Contains(strings.ToLower(pkg), strings.ToLower(f)) {
		fail = "1"
	}
	label := verb + " " + pkg
	if version != "" {
		label += " " + version
	}
	return [][]string{{exe, "demo-step", label, "2.4", fail}}
}

func (d *demoBackend) Install(p, v string) [][]string { return d.step("Installing", p, v) }
func (d *demoBackend) Uninstall(p string) [][]string  { return d.step("Removing", p, "") }
func (d *demoBackend) Upgrade(p, v string) [][]string {
	if p == "" {
		return d.step("Updating", "all packages", "")
	}
	return d.step("Updating", p, v)
}
func (d *demoBackend) Pin(p string) [][]string   { return [][]string{{"echo", "pin", p}} }
func (d *demoBackend) Unpin(p string) [][]string { return [][]string{{"echo", "unpin", p}} }

// Apply keeps the simulated machine state in step with finished jobs.
func (d *demoBackend) Apply(s Step, ok bool) {
	if !ok {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch s.Type {
	case "install", "upgrade":
		if s.Pkg != "" {
			v := s.Version
			if v == "" {
				v = "latest"
			}
			d.installed[s.Pkg] = v
		}
	case "uninstall":
		delete(d.installed, s.Pkg)
	case "pin":
		d.pinned[s.Pkg] = true
	case "unpin":
		delete(d.pinned, s.Pkg)
	}
}

func (d *demoBackend) Inventory(progress func(string)) catalog.Inventory {
	inv := newInventory()
	for _, st := range []string{"Reading installed apps", "Checking for updates", "Reading held packages"} {
		stage(progress, st)
		time.Sleep(450 * time.Millisecond)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, v := range d.installed {
		inv.Installed[k] = v
	}
	inv.Names["Vivaldi"] = "8.2.4133.80"
	// a few packages outside the catalog, so "All installed software" has something to show
	for id, v := range map[string][2]string{"Microsoft.Edge": {"Microsoft Edge", "129.0.2792.65"}, "Microsoft.OneDrive": {"Microsoft OneDrive", "24.180.0905"},
		"Logitech.OptionsPlus": {"Logi Options+", "1.80.1"}, "Microsoft.AppInstaller": {"App Installer", "1.23.1911"}} {
		inv.Installed[id], inv.Labels[id] = v[1], v[0]
	}
	inv.Upgradable["Logitech.OptionsPlus"] = "1.82.6"
	inv.Upgradable["VideoLAN.VLC"] = "3.0.21"
	inv.Upgradable["Mozilla.Firefox"] = "129.0"
	for k := range d.pinned {
		inv.Pinned[k] = d.installed[k]
	}
	return inv
}

// cmdDemoStep is the hidden child process: prints a believable install transcript over `seconds`, then exits.
func cmdDemoStep(args []string) {
	if len(args) < 3 {
		os.Exit(2)
	}
	label := args[0]
	secs, _ := strconv.ParseFloat(args[1], 64)
	fail := args[2] == "1"
	pace := time.Duration(secs / 6 * float64(time.Second))
	p := func(s string) { fmt.Println(s); time.Sleep(pace) }
	p("Found " + label)
	p("This application is licensed to you by its owner.")
	p("Downloading https://cdn.winget.microsoft.com/cache/demo/installer.exe")
	for i := 0; i < 3; i++ { // progress bars redraw with \r and must be filtered out of the log
		fmt.Printf("  %s%s  %d%%\r", strings.Repeat("█", i*3+2), strings.Repeat("▒", 8-i*3), 30+i*30)
		time.Sleep(pace / 2)
	}
	fmt.Println()
	p("Successfully verified installer hash")
	p("Starting package install...")
	if fail {
		fmt.Println("Installer failed with exit code: 1603")
		os.Exit(1603)
	}
	fmt.Println("Successfully installed")
}

// Applier is implemented by backends that mirror job results in a simulated machine (the demo backend).
type Applier interface{ Apply(s Step, ok bool) }
