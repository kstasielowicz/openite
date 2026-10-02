// openite: one static binary with no dependencies. It is a CLI installer (install/update/pick),
// a local web UI (ui) and the fleet agent (enroll/run/install-service). Run `openite help`.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openite/openite/catalog"
)

// longPolled is true when the last poll was already held open by the server, so the loop can poll again at once.
var longPolled atomic.Bool

// pollInterval is the server-provided seconds between polls (0 = use the --interval flag).
var pollInterval atomic.Int64

var version = "0.7.1" // overridden at release time: -ldflags "-X main.version=..."

type Config struct {
	Server   string `json:"server"`
	Token    string `json:"token"`
	DeviceID int    `json:"device_id"`
}

type Step struct {
	Type    string `json:"type"`
	Key     string `json:"key"` // catalog key, so a UI can show progress on the right app card
	App     string `json:"app"`
	Pkg     string `json:"pkg"`
	All     bool   `json:"all"`
	Name    string `json:"name"`
	Shell   string `json:"shell"`
	Body    string `json:"body"`
	Reason  string `json:"reason"`
	Version string `json:"version"` // install/upgrade this exact version (empty = latest)
	// Aliases are the display names the app has in "installed programs"; used for headless uninstall.
	Aliases []string `json:"aliases"`
}

type Job struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Steps []Step `json:"steps"`
}

func defaultConfig() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "openite", "agent.json")
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

// ---- server API ------------------------------------------------------------

var httpClient = &http.Client{Timeout: 30 * time.Second}

func api(cfg Config, method, path string, in any, out any, auth bool) error {
	var body io.Reader
	if method != "GET" {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(cfg.Server, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		var e struct{ Error string }
		json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%d: %s", resp.StatusCode, e.Error)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func osName() string { return runtime.GOOS + "/" + runtime.GOARCH }

func checkin(cfg Config, b Backend) {
	inv := b.Inventory(nil)
	err := api(cfg, "POST", "/api/agent/checkin", map[string]any{
		"installed": inv.Installed, "upgradable": inv.Upgradable,
		"installed_names": inv.Names, "upgradable_names": inv.UpgradableNames,
		"pinned": inv.Pinned, "pinned_names": inv.PinnedNames, "sysinfo": currentSysInfo(),
		"manager": b.Name(), "os": osName(), "agent_version": version,
	}, nil, true)
	if err != nil {
		fmt.Println("check-in failed:", err)
	}
}

// ---- job execution ---------------------------------------------------------

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func runScript(s Step, onLine func(string)) (int, string) {
	ext, argv := "", []string(nil)
	switch s.Shell {
	case "powershell":
		ext = ".ps1"
		ps := "powershell"
		if have("pwsh") {
			ps = "pwsh"
		}
		argv = []string{ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File"}
	case "bash":
		ext, argv = ".sh", []string{"bash"}
	case "cmd":
		ext, argv = ".cmd", []string{"cmd", "/c"}
	default:
		return 1, "unknown shell " + s.Shell
	}
	f, err := os.CreateTemp("", "openite-*"+ext)
	if err != nil {
		return -1, err.Error()
	}
	defer os.Remove(f.Name())
	f.WriteString(s.Body)
	f.Close()
	return runLive(append(argv, f.Name()), nil, time.Hour, onLine)
}

// jobHooks lets a caller watch a job while it runs: raw output lines, and a status for each step.
type jobHooks struct {
	Line func(string)
	Step func(i int, status, hint string) // status: running | done | failed | skipped
}

func (h jobHooks) line(s string) {
	if h.Line != nil {
		h.Line(s)
	}
}

func (h jobHooks) step(i int, status, hint string) {
	if h.Step != nil {
		h.Step(i, status, hint)
	}
}

// executeJob runs every step in order and reports progress through hk as it happens.
func executeJob(job Job, b Backend, hk jobHooks) string {
	failed := false
	for n, s := range job.Steps {
		label := map[string]string{"install": "install " + s.App, "uninstall": "uninstall " + s.App,
			"upgrade": "upgrade " + s.App, "script": "script " + s.Name, "skip": "skip " + s.App,
			"pin": "hold " + s.App, "unpin": "release hold on " + s.App}[s.Type]
		if s.Version != "" && (s.Type == "install" || s.Type == "upgrade") {
			label += " (version " + s.Version + ")"
		}
		if s.Type == "upgrade" && s.All {
			label = "upgrade all"
		}
		hk.line(fmt.Sprintf("=== [%d/%d] %s", n+1, len(job.Steps), label))
		if s.Type == "skip" {
			hk.line("skipped: " + s.Reason)
			hk.step(n, "skipped", s.Reason)
			continue
		}
		hk.step(n, "running", "")
		var code int
		var out string
		good := true
		if s.Type == "script" {
			code, out = runScript(s, hk.Line)
			good = code == 0
		} else {
			pkg := s.Pkg
			if s.All {
				pkg = ""
			}
			var cmds [][]string
			switch {
			case s.Type == "uninstall" && b.Name() == "winget" && runtime.GOOS == "windows" && os.Getenv("OPENITE_DEMO") == "" && (pkg == "" || catalog.SafeID.MatchString(pkg)):
				code, out = smartUninstall(s, b)
				good = b.OK(code)
				hk.line(strings.TrimSpace(out))
			case pkg != "" && !catalog.SafeID.MatchString(pkg): // never hand odd-looking ids (e.g. "--flag") to a package manager
				good, out = false, fmt.Sprintf("refusing unsafe package id %q", pkg)
				hk.line(out)
			case s.Version != "" && !catalog.SafeVersion.MatchString(s.Version):
				good, out = false, fmt.Sprintf("refusing unsafe version %q", s.Version)
				hk.line(out)
			case s.Version != "" && !b.SupportsVersions():
				good, out = false, b.Name()+" cannot install a specific version"
				hk.line(out)
			case s.Type == "install":
				cmds = b.Install(pkg, s.Version)
			case s.Type == "uninstall":
				cmds = b.Uninstall(pkg)
			case s.Type == "upgrade":
				cmds = b.Upgrade(pkg, s.Version)
			case s.Type == "pin" && pkg != "":
				cmds = b.Pin(pkg)
			case s.Type == "unpin" && pkg != "":
				cmds = b.Unpin(pkg)
			default:
				good, out = false, "unknown step type "+s.Type
				hk.line(out)
			}
			for _, c := range cmds {
				var o string
				code, o = runLive(c, b.Env(), time.Hour, hk.Line)
				out += o + "\n"
				if !b.OK(code) {
					good = false
					break
				}
			}
		}
		if ap, ok := b.(Applier); ok {
			ap.Apply(s, good)
		}
		if good {
			hk.line("-> ok")
			hk.step(n, "done", "")
		} else {
			hk.line(fmt.Sprintf("-> FAILED (exit %d)", code))
			hint := explainFailure(code, out)
			if hint != "" {
				hk.line("hint: " + hint)
			}
			hk.step(n, "failed", hint)
			failed = true
		}
	}
	if failed {
		return "failed"
	}
	return "done"
}

// progressSender streams a running job to the server about once a second (live log plus per-step status),
// so the web UI shows installs as they happen. The final result still goes through /result.
type progressSender struct {
	cfg   Config
	id    int
	mu    sync.Mutex
	lines []string
	steps map[int]map[string]string
	dirty bool
	stop  chan struct{}
	done  chan struct{}
}

func newProgressSender(cfg Config, id int) *progressSender {
	p := &progressSender{cfg: cfg, id: id, steps: map[int]map[string]string{}, stop: make(chan struct{}), done: make(chan struct{})}
	go p.loop()
	return p
}

func (p *progressSender) hooks() jobHooks {
	return jobHooks{
		Line: func(l string) { p.mu.Lock(); p.lines = append(p.lines, l); p.dirty = true; p.mu.Unlock() },
		Step: func(i int, status, hint string) {
			p.mu.Lock()
			p.steps[i] = map[string]string{"status": status, "hint": hint}
			p.dirty = true
			p.mu.Unlock()
		},
	}
}

func (p *progressSender) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.lines, "\n")
}

func (p *progressSender) flush() {
	p.mu.Lock()
	if !p.dirty {
		p.mu.Unlock()
		return
	}
	steps := make([]map[string]any, 0, len(p.steps))
	for i, s := range p.steps {
		steps = append(steps, map[string]any{"i": i, "status": s["status"], "hint": s["hint"]})
	}
	log := tail(strings.Join(p.lines, "\n"), 60000)
	p.dirty = false
	p.mu.Unlock()
	api(p.cfg, "POST", fmt.Sprintf("/api/agent/jobs/%d/progress", p.id), map[string]any{"log": log, "steps": steps}, nil, true) // best effort
}

func (p *progressSender) loop() {
	defer close(p.done)
	t := time.NewTicker(1200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			p.flush()
		case <-p.stop:
			return
		}
	}
}

// finish stops the ticker and sends whatever is still pending, so the last step's status always reaches the server.
func (p *progressSender) finish() {
	close(p.stop)
	<-p.done
	p.flush()
}

func processOnce(cfg Config, b Backend, longPoll bool) (bool, error) {
	var res struct {
		Job      *Job
		Interval int
		Long     bool
	}
	path := "/api/agent/poll"
	if longPoll {
		path += "?wait=20" // the server holds the request until work arrives; older servers ignore it and answer at once
	}
	if err := api(cfg, "GET", path, nil, &res, true); err != nil {
		return false, err
	}
	longPolled.Store(res.Long)
	if res.Interval >= 5 && res.Interval <= 300 {
		pollInterval.Store(int64(res.Interval)) // the server decides how often devices check in
	}
	if res.Job == nil {
		return false, nil
	}
	fmt.Printf("job %d: %s\n", res.Job.ID, res.Job.Title)
	ps := newProgressSender(cfg, res.Job.ID)
	status := executeJob(*res.Job, b, ps.hooks())
	ps.finish()
	err := api(cfg, "POST", fmt.Sprintf("/api/agent/jobs/%d/result", res.Job.ID), map[string]string{"status": status, "log": ps.text()}, nil, true)
	checkin(cfg, b)
	return true, err
}

// ---- commands --------------------------------------------------------------

func loadConfig(path string) Config {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		die("No config at %s. Run 'enroll' first.", path)
	}
	if json.Unmarshal(data, &cfg) != nil || cfg.Token == "" {
		die("Config %s is invalid.", path)
	}
	return cfg
}

func cmdEnroll(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", "", "server URL, e.g. http://192.168.1.10:8080")
	code := fs.String("code", "", "enrollment code from the web UI")
	name := fs.String("name", "", "device name (default: hostname)")
	cfgPath := fs.String("config", defaultConfig(), "config file")
	insecure := fs.Bool("allow-insecure", false, "allow plain http:// to a public address (not recommended)")
	fs.Parse(args)
	if err := checkServerURL(*server, *insecure); err != nil {
		die("%v", err)
	}
	if *server == "" || *code == "" {
		die("usage: openite enroll --server URL --code CODE")
	}
	b := pickBackend()
	if b == nil {
		die("No supported package manager found (need winget, brew or apt-get).")
	}
	if *name == "" {
		*name, _ = os.Hostname()
	}
	cfg := Config{Server: *server}
	var res struct {
		DeviceID int    `json:"device_id"`
		Token    string `json:"token"`
	}
	if err := api(cfg, "POST", "/api/agent/enroll", map[string]string{"code": *code, "name": *name, "os": osName(), "manager": b.Name()}, &res, false); err != nil {
		die("Enrollment failed: %v", err)
	}
	cfg.Token, cfg.DeviceID = res.Token, res.DeviceID
	abs, _ := filepath.Abs(*cfgPath)
	os.MkdirAll(filepath.Dir(abs), 0o755)
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(abs, data, 0o600); err != nil {
		die("cannot write config: %v", err)
	}
	fmt.Printf("Enrolled as device %d using %s. Config: %s\nNext: openite run   (or: openite install-service)\n", res.DeviceID, b.Name(), abs)
	checkin(cfg, b)
}

func cmdRun(args []string, loop bool) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfig(), "config file")
	interval := fs.Int("interval", 15, "seconds between polls")
	fs.Parse(args)
	cfg := loadConfig(*cfgPath)
	b := pickBackend()
	if b == nil {
		die("No supported package manager found.")
	}
	var lastCheckin time.Time
	failures := 0
	for {
		var err error
		if time.Since(lastCheckin) > 10*time.Minute {
			checkin(cfg, b)
			lastCheckin = time.Now()
		}
		for {
			var did bool
			did, err = processOnce(cfg, b, loop) // `once` must never wait
			if err != nil || !did {
				break
			}
		}
		if err != nil {
			failures++
			fmt.Println("error:", err)
		} else {
			failures = 0
		}
		if !loop {
			return
		}
		if err == nil && longPolled.Load() {
			time.Sleep(300 * time.Millisecond) // the server already made us wait; ask again right away
			continue
		}
		// Jitter so thousands of agents don't hit the server in lockstep; back off while the server is unreachable.
		d := time.Duration(*interval) * time.Second
		if v := pollInterval.Load(); v >= 5 {
			d = time.Duration(v) * time.Second
		}
		for i := 0; i < failures && d < 5*time.Minute; i++ {
			d *= 2
		}
		time.Sleep(d + time.Duration(rand.Int63n(int64(d)/5+1)))
	}
}

func main() {
	initTerminal()
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"ui"} // double-clicking the exe opens the UI
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "enroll":
		cmdEnroll(rest)
	case "run":
		cmdRun(rest, true)
	case "once":
		cmdRun(rest, false)
	case "install-service":
		cmdService(rest, true)
	case "uninstall-service":
		cmdService(rest, false)
	case "ui":
		cmdUI(rest)
	case "pick":
		cmdPick(rest)
	case "list":
		cmdList(rest)
	case "search":
		cmdSearch(rest)
	case "presets":
		cmdPresets(rest)
	case "status":
		cmdStatus(rest)
	case "drivers":
		cmdDrivers(rest)
	case "versions":
		cmdVersions(rest)
	case "demo-step": // hidden: child process of the demo backend
		cmdDemoStep(rest)
	case "schedule":
		cmdSchedule(rest)
	case "install":
		cmdChange("install", rest)
	case "update", "upgrade":
		cmdChange("upgrade", rest)
	case "uninstall":
		cmdChange("uninstall", rest)
	case "hold", "pin":
		cmdChange("pin", rest)
	case "unhold", "unpin":
		cmdChange("unpin", rest)
	case "version", "--version", "-v":
		fmt.Println("openite", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}
