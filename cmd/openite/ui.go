package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openite/openite/catalog"
	"github.com/openite/openite/web"
)

// Local single-PC web UI (`openite ui`). Speaks the same JSON API subset as the fleet server,
// so web/index.html works unchanged. Loopback only, no accounts.

type uiJob struct {
	ID       int        `json:"id"`
	DeviceID int        `json:"device_id"`
	Title    string     `json:"title"`
	Status   string     `json:"status"`
	Log      string     `json:"log"`
	Created  float64    `json:"created"`
	Started  *float64   `json:"started"`
	Finished *float64   `json:"finished"`
	Source   string     `json:"source"`
	Steps    []stepView `json:"steps"` // live status of every step, for progress on app cards
	steps    []Step
}

// stepView is what the UI sees of a step: enough to show "Installing Git" and why something failed.
type stepView struct {
	Key     string `json:"key"`
	App     string `json:"app"`
	Type    string `json:"type"`
	Version string `json:"version,omitempty"`
	Status  string `json:"status"` // queued | running | done | failed | skipped
	Hint    string `json:"hint,omitempty"`
}

// hub fans "something changed" out to every open /api/events stream (server-sent events).
type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newHub() *hub { return &hub{subs: map[chan struct{}]struct{}{}} }

func (h *hub) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *hub) notify() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default: // already has a pending wake-up; the client will refetch once and see everything
		}
	}
}

type syncStep struct {
	Name   string `json:"name"`
	Status string `json:"status"` // running | done
}

type localUI struct {
	b      Backend
	token  string
	mu     sync.Mutex
	jobs   []*uiJob
	inv    catalog.Inventory
	sys    SysInfo
	queue  chan *uiJob
	hw     Hardware
	advice []Advice
	hub    *hub

	// sync progress, shown as a loader in the UI and as a spinner label in the terminal
	syncMu     sync.Mutex
	syncSteps  []syncStep
	syncBusy   bool
	everSynced bool
	syncedAt   float64
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func ptr(f float64) *float64 { return &f }

// syncStage marks the previous stage done and the named one running.
func (u *localUI) syncStage(name string) {
	u.syncMu.Lock()
	defer u.syncMu.Unlock()
	for i := range u.syncSteps {
		u.syncSteps[i].Status = "done"
	}
	u.syncSteps = append(u.syncSteps, syncStep{name, "running"})
	u.hub.notify()
}

func (u *localUI) currentStage() string {
	u.syncMu.Lock()
	defer u.syncMu.Unlock()
	if len(u.syncSteps) == 0 {
		return "Connecting to " + u.b.Name()
	}
	return u.syncSteps[len(u.syncSteps)-1].Name
}

// sync rereads the package manager and system details. Safe to call repeatedly; overlapping calls are ignored.
func (u *localUI) sync() {
	u.syncMu.Lock()
	if u.syncBusy {
		u.syncMu.Unlock()
		return
	}
	u.syncBusy, u.syncSteps = true, nil
	u.syncMu.Unlock()
	u.hub.notify()

	inv := u.b.Inventory(u.syncStage)
	u.syncStage("Reading system details")
	sys := collectSysInfo()
	sysMu.Lock() // share the result with check-ins
	sysCache, sysAt = sys, time.Now()
	sysMu.Unlock()
	hw := Hardware{GPUs: sys.GPUs, Maker: sys.Maker, Model: sys.Model, CPU: sys.CPU}

	u.mu.Lock()
	u.inv, u.sys, u.hw, u.advice, u.syncedAt = inv, sys, hw, driverAdvice(hw), now()
	u.mu.Unlock()

	u.syncMu.Lock()
	for i := range u.syncSteps {
		u.syncSteps[i].Status = "done"
	}
	u.syncBusy, u.everSynced = false, true
	u.syncMu.Unlock()
	u.hub.notify()
}

func (u *localUI) worker() {
	for j := range u.queue {
		u.mu.Lock()
		if j.Status == "cancelled" {
			u.mu.Unlock()
			continue
		}
		j.Status, j.Started = "running", ptr(now())
		u.mu.Unlock()
		u.hub.notify()
		hk := jobHooks{
			Line: func(l string) {
				u.mu.Lock()
				if j.Log != "" {
					j.Log += "\n"
				}
				j.Log += l
				u.mu.Unlock()
				u.hub.notify()
			},
			Step: func(i int, status, hint string) {
				u.mu.Lock()
				if i >= 0 && i < len(j.Steps) {
					j.Steps[i].Status, j.Steps[i].Hint = status, hint
				}
				u.mu.Unlock()
				u.hub.notify()
			},
		}
		st := executeJob(Job{ID: j.ID, Title: j.Title, Steps: j.steps}, u.b, hk)
		u.mu.Lock()
		j.Status, j.Finished = st, ptr(now())
		u.mu.Unlock()
		u.hub.notify()
		u.sync()
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

var hostOK = regexp.MustCompile(`^(localhost|127\.0\.0\.1|\[::1\])(:\d+)?$`)
var iconName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*\.svg$`)

func catalogHas(key, manager string) bool {
	a, ok := catalog.Find(key)
	return ok && a.Pkg(manager) != ""
}

func (u *localUI) device() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	have, outdated, held := []string{}, []string{}, []string{}
	versions, available := map[string]string{}, map[string]string{}
	for _, a := range catalog.Apps() {
		st := a.Status(u.b.Name(), u.inv)
		if st.Installed {
			have = append(have, a.Key)
			if st.Version != "" {
				versions[a.Key] = st.Version
			}
		}
		if st.Upgradable {
			outdated = append(outdated, a.Key)
			available[a.Key] = st.Available
		}
		if st.Held {
			held = append(held, a.Key)
		}
	}
	// every package the manager reports, catalog or not, so the UI can show the whole machine
	owner := map[string]string{}
	for _, a := range catalog.Apps() {
		if p := a.Pkg(u.b.Name()); p != "" {
			owner[strings.ToLower(p)] = a.Key
		}
	}
	pkgs := []map[string]any{}
	for id, v := range u.inv.Installed {
		key := owner[strings.ToLower(id)]
		name := u.inv.Labels[id]
		if a, ok := catalog.Find(key); name == "" && ok {
			name = a.Name
		}
		if name == "" {
			name = id
		}
		_, held := u.inv.Pinned[id]
		pkgs = append(pkgs, map[string]any{"id": id, "name": name, "version": v, "available": u.inv.Upgradable[id], "held": held, "key": key})
	}
	sort.Slice(pkgs, func(i, j int) bool {
		return strings.ToLower(pkgs[i]["name"].(string)) < strings.ToLower(pkgs[j]["name"].(string))
	})
	sys := u.sys
	if sys.BootUnix > 0 {
		sys.UptimeSec = time.Now().Unix() - sys.BootUnix
	}
	host, _ := os.Hostname()
	return map[string]any{"id": 1, "name": host + " (this PC)", "os": osName(), "manager": u.b.Name(), "tags": []string{},
		"last_seen": now(), "online": true, "have": have, "outdated": outdated, "held": held, "versions": versions,
		"available": available, "packages": pkgs, "synced_at": u.syncedAt, "n_installed": len(u.inv.Installed), "n_upgradable": len(u.inv.Upgradable),
		"managed": false, "missing": []string{}, "sysinfo": sys, "agent_version": version, "notes": "", "settings": map[string]any{}}
}

func (u *localUI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			fail(w, 404, "not found")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		w.Header().Set("Cache-Control", "no-cache") // a new openite version must never show yesterday's page
		w.Write(web.Index)
	})
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"home": true, "lite": true, "registration": false, "version": version, "manager": u.b.Name()})
	})
	mux.HandleFunc("/api/home", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"token": u.token, "email": "This PC", "home": true})
	})
	mux.HandleFunc("/api/sync", func(w http.ResponseWriter, r *http.Request) {
		u.syncMu.Lock()
		defer u.syncMu.Unlock()
		writeJSON(w, 200, map[string]any{"ready": u.everSynced, "busy": u.syncBusy, "steps": u.syncSteps, "manager": u.b.Name()})
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			fail(w, 500, "streaming not supported")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		ch := u.hub.subscribe()
		defer u.hub.unsubscribe(ch)
		fmt.Fprint(w, "retry: 2000\n\nevent: hello\ndata: {}\n\n")
		fl.Flush()
		beat := time.NewTicker(15 * time.Second)
		defer beat.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ch:
				fmt.Fprint(w, "event: change\ndata: {}\n\n")
				fl.Flush()
			case <-beat.C:
				fmt.Fprint(w, ": keepalive\n\n")
				fl.Flush()
			}
		}
	})
	mux.HandleFunc("/api/rescan", func(w http.ResponseWriter, r *http.Request) {
		go u.sync()
		writeJSON(w, 200, map[string]any{})
	})
	mux.HandleFunc("/api/catalog", func(w http.ResponseWriter, r *http.Request) {
		apps := []catalog.App{} // only what this PC's package manager can actually install
		for _, a := range catalog.Apps() {
			if a.Pkg(u.b.Name()) != "" {
				apps = append(apps, a)
			}
		}
		writeJSON(w, 200, apps)
	})
	mux.HandleFunc("/api/icons", func(w http.ResponseWriter, r *http.Request) {
		keys := []string{}
		if ents, err := web.Files.ReadDir("icons"); err == nil {
			for _, e := range ents {
				keys = append(keys, strings.TrimSuffix(e.Name(), ".svg"))
			}
		}
		writeJSON(w, 200, keys)
	})
	mux.HandleFunc("/api/packs", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, catalog.Packs()) })
	mux.HandleFunc("/icons/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/icons/")
		if !iconName.MatchString(name) {
			fail(w, 404, "not found")
			return
		}
		data, err := web.Files.ReadFile("icons/" + name)
		if err != nil {
			fail(w, 404, "not found")
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(data)
	})
	mux.HandleFunc("/api/hardware", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		adv := []Advice{}
		for _, a := range u.advice {
			if a.AppKey == "" || catalogHas(a.AppKey, u.b.Name()) {
				adv = append(adv, a)
			}
		}
		writeJSON(w, 200, map[string]any{"hardware": u.hw, "advice": adv})
	})
	mux.HandleFunc("/api/drivers/open", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Title string }
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
		u.mu.Lock()
		defer u.mu.Unlock()
		for _, a := range u.advice { // only fixed vendor URLs from our own advice list can be opened
			if a.Title == body.Title && a.URL != "" {
				openBrowser(a.URL)
				writeJSON(w, 200, map[string]any{})
				return
			}
		}
		fail(w, 404, "unknown item")
	})
	mux.HandleFunc("/api/versions", func(w http.ResponseWriter, r *http.Request) {
		a, ok := catalog.Find(r.URL.Query().Get("key"))
		if !ok {
			fail(w, 404, "unknown app")
			return
		}
		if !u.b.SupportsVersions() {
			writeJSON(w, 200, map[string]any{"supported": false, "versions": []string{}})
			return
		}
		vs, err := versionsFor(u.b, a)
		if err != nil {
			fail(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"supported": true, "versions": vs})
	})
	mux.HandleFunc("/api/devices", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, []any{u.device()}) })
	mux.HandleFunc("/api/profiles", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, []any{}) })
	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		if r.Method == "GET" {
			out := []*uiJob{}
			for i := len(u.jobs) - 1; i >= 0 && len(out) < 100; i-- {
				out = append(out, u.jobs[i])
			}
			writeJSON(w, 200, out)
			return
		}
		var body struct {
			Title string `json:"title"`
			Steps []struct {
				Type     string            `json:"type"`
				Apps     any               `json:"apps"`
				Versions map[string]string `json:"versions"`
			} `json:"steps"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body) != nil || len(body.Steps) == 0 {
			fail(w, 400, "invalid request")
			return
		}
		var steps []Step
		for _, s := range body.Steps {
			switch s.Type {
			case "install", "upgrade", "uninstall", "pin", "unpin":
			default:
				fail(w, 400, "this mode supports install, update, uninstall and hold only")
				return
			}
			if s.Apps == "all" && s.Type == "upgrade" {
				steps = append(steps, Step{Type: "upgrade", All: true})
				continue
			}
			keys, _ := s.Apps.([]any)
			for _, k := range keys {
				// "pkg:<id>" targets a package outside the catalog; only ones this machine reports, and never for install
				if id, isPkg := strings.CutPrefix(fmt.Sprint(k), "pkg:"); isPkg {
					if _, have := u.inv.Installed[id]; !have || s.Type == "install" || !catalog.SafeID.MatchString(id) {
						fail(w, 400, fmt.Sprintf("unknown package: %s", id))
						return
					}
					name := u.inv.Labels[id]
					if name == "" {
						name = id
					}
					steps = append(steps, Step{Type: s.Type, Key: "pkg:" + id, App: name, Pkg: id, Aliases: []string{name}})
					continue
				}
				a, ok := catalog.Find(fmt.Sprint(k))
				if !ok {
					fail(w, 400, fmt.Sprintf("unknown app: %v", k))
					return
				}
				ver := s.Versions[a.Key]
				if ver != "" && !catalog.SafeVersion.MatchString(ver) {
					fail(w, 400, fmt.Sprintf("invalid version for %s", a.Name))
					return
				}
				if pkg := a.Pkg(u.b.Name()); pkg != "" {
					steps = append(steps, stepFor(s.Type, a, pkg, ver))
				} else {
					steps = append(steps, Step{Type: "skip", App: a.Name, Reason: "no " + u.b.Name() + " package id"})
				}
			}
		}
		views := make([]stepView, len(steps))
		for i, st := range steps {
			views[i] = stepView{Key: st.Key, App: st.App, Type: st.Type, Version: st.Version, Status: "queued"}
			if st.Type == "skip" {
				views[i].Status, views[i].Hint = "skipped", st.Reason
			}
		}
		j := &uiJob{ID: len(u.jobs) + 1, DeviceID: 1, Title: body.Title, Status: "queued", Created: now(), Source: "manual", steps: steps, Steps: views}
		u.jobs = append(u.jobs, j)
		u.queue <- j
		defer u.hub.notify()
		writeJSON(w, 200, map[string]any{"job_ids": []int{j.ID}})
	})
	mux.HandleFunc("/api/jobs/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // api jobs <id> cancel
		if len(parts) != 4 || parts[3] != "cancel" {
			fail(w, 404, "not found")
			return
		}
		id, err := strconv.Atoi(parts[2])
		if err != nil {
			fail(w, 404, "not found")
			return
		}
		u.mu.Lock()
		defer u.mu.Unlock()
		if id >= 1 && id <= len(u.jobs) && u.jobs[id-1].Status == "queued" {
			u.jobs[id-1].Status, u.jobs[id-1].Finished = "cancelled", ptr(now())
		}
		writeJSON(w, 200, map[string]any{})
	})
	return u.guard(mux)
}

// guard blocks DNS-rebinding (Host must be localhost) and requires the per-run token on /api/* (except the
// endpoints that bootstrap it). A random website can't read /api/home (no CORS) so it never learns the token.
func (u *localUI) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostOK.MatchString(r.Host) {
			fail(w, 403, "forbidden host")
			return
		}
		open := r.URL.Path == "/api/info" || r.URL.Path == "/api/home" || r.URL.Path == "/api/sync"
		if strings.HasPrefix(r.URL.Path, "/api/") && !open {
			if r.Header.Get("Authorization") != "Bearer "+u.token {
				fail(w, 401, "not signed in")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start()
}

func cmdUI(args []string) {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	port := fs.Int("port", 8080, "port to listen on (next free port is used if busy)")
	noBrowser := fs.Bool("no-browser", false, "don't open a browser window")
	dry := fs.Bool("dry-run", false, "show commands instead of running them")
	fs.Parse(args)
	if *dry {
		dryRun = true
	}
	b := needBackend()
	var tok [16]byte
	rand.Read(tok[:])
	u := &localUI{b: b, token: hex.EncodeToString(tok[:]), queue: make(chan *uiJob, 64), hub: newHub()}
	var ln net.Listener
	var err error
	for p := *port; p < *port+20; p++ {
		if ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			break
		}
	}
	if ln == nil {
		die("could not open a local port: %v", err)
	}
	url := fmt.Sprintf("http://localhost:%d", ln.Addr().(*net.TCPAddr).Port)
	srv := &http.Server{Handler: u.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { die("%v", srv.Serve(ln)) }()
	go u.worker()

	// Read the machine first, with live progress, so the page opens fully populated.
	banner("local interface")
	fmt.Println()
	withSpinnerDyn(u.currentStage, func() bool { u.sync(); return true })
	d := u.device()
	fmt.Printf("  %s %s\n\n", dim("Found"), dim(fmt.Sprintf("%d packages, %d updates available", d["n_installed"], d["n_upgradable"])))
	fmt.Printf("  %s %s\n  %s\n\n", bold("Openite is running at"), cyan(url), dim("Keep this window open while you use it. Press Ctrl+C to quit."))
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			u.sync()
		}
	}()
	if !*noBrowser {
		openBrowser(url)
	}
	select {}
}
