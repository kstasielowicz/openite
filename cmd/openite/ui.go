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
	ID       int      `json:"id"`
	DeviceID int      `json:"device_id"`
	Title    string   `json:"title"`
	Status   string   `json:"status"`
	Log      string   `json:"log"`
	Created  float64  `json:"created"`
	Started  *float64 `json:"started"`
	Finished *float64 `json:"finished"`
	Source   string   `json:"source"`
	steps    []Step
}

type localUI struct {
	b      Backend
	token  string
	mu     sync.Mutex
	jobs   []*uiJob
	inv    catalog.Inventory
	queue  chan *uiJob
	hw     Hardware
	advice []Advice
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func (u *localUI) refresh() {
	inv := u.b.Inventory()
	u.mu.Lock()
	u.inv = inv
	u.mu.Unlock()
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
		var lines []string
		st := executeJob(Job{ID: j.ID, Title: j.Title, Steps: j.steps}, u.b, func(l string) { lines = append(lines, l) })
		u.mu.Lock()
		j.Status, j.Log, j.Finished = st, strings.Join(lines, "\n"), ptr(now())
		u.mu.Unlock()
		u.refresh()
	}
}

func ptr(f float64) *float64 { return &f }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

var iconName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*\.svg$`)

func catalogHas(key, manager string) bool {
	a, ok := catalog.Find(key)
	return ok && a.Pkg(manager) != ""
}

var hostOK = regexp.MustCompile(`^(localhost|127\.0\.0\.1|\[::1\])(:\d+)?$`)

func (u *localUI) device() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	have, outdated := []string{}, []string{}
	for _, a := range catalog.Apps() {
		in, old := a.State(u.b.Name(), u.inv)
		if in {
			have = append(have, a.Key)
		}
		if old {
			outdated = append(outdated, a.Key)
		}
	}
	host, _ := os.Hostname()
	return map[string]any{"id": 1, "name": host + " (this PC)", "os": osName(), "manager": u.b.Name(), "tags": []string{},
		"last_seen": now(), "online": true, "have": have, "outdated": outdated,
		"n_installed": len(u.inv.Installed), "n_upgradable": len(u.inv.Upgradable), "managed": false, "missing": []string{}}
}

func (u *localUI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			fail(w, 404, "not found")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		w.Write(web.Index)
	})
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"home": true, "lite": true, "registration": false, "version": version})
	})
	mux.HandleFunc("/api/home", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"token": u.token, "email": "This PC", "home": true})
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
	mux.HandleFunc("/api/packs", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, catalog.Packs()) })
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
				Type string `json:"type"`
				Apps any    `json:"apps"`
			} `json:"steps"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body) != nil || len(body.Steps) == 0 {
			fail(w, 400, "invalid request")
			return
		}
		var steps []Step
		for _, s := range body.Steps {
			if s.Type != "install" && s.Type != "upgrade" && s.Type != "uninstall" {
				fail(w, 400, "this mode supports install, update and uninstall only")
				return
			}
			if s.Apps == "all" && s.Type == "upgrade" {
				steps = append(steps, Step{Type: "upgrade", All: true})
				continue
			}
			keys, _ := s.Apps.([]any)
			for _, k := range keys {
				a, ok := catalog.Find(fmt.Sprint(k))
				if !ok {
					fail(w, 400, fmt.Sprintf("unknown app: %v", k))
					return
				}
				if pkg := a.Pkg(u.b.Name()); pkg != "" {
					steps = append(steps, stepFor(s.Type, a, pkg))
				} else {
					steps = append(steps, Step{Type: "skip", App: a.Name, Reason: "no " + u.b.Name() + " package id"})
				}
			}
		}
		j := &uiJob{ID: len(u.jobs) + 1, DeviceID: 1, Title: body.Title, Status: "queued", Created: now(), Source: "manual", steps: steps}
		u.jobs = append(u.jobs, j)
		u.queue <- j
		writeJSON(w, 200, map[string]any{"job_ids": []int{j.ID}})
	})
	mux.HandleFunc("/api/jobs/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // api jobs <id> cancel
		id, err := strconv.Atoi(parts[2])
		if err != nil || len(parts) != 4 || parts[3] != "cancel" {
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
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/info" && r.URL.Path != "/api/home" {
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
	u := &localUI{b: b, token: hex.EncodeToString(tok[:]), queue: make(chan *uiJob, 64)}
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
	fmt.Printf("Openite is running at %s\nKeep this window open while you use it. Press Ctrl+C to quit.\n", url)
	go u.worker()
	go func() {
		h := detectHardware()
		u.mu.Lock()
		u.hw, u.advice = h, driverAdvice(h)
		u.mu.Unlock()
	}()
	go func() {
		for {
			u.refresh()
			time.Sleep(5 * time.Minute)
		}
	}()
	if !*noBrowser {
		openBrowser(url)
	}
	srv := &http.Server{Handler: u.handler(), ReadHeaderTimeout: 10 * time.Second}
	die("%v", srv.Serve(ln))
}
