package main

import (
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The demo backend runs "demo-step" as a child of the running executable, which in tests is the test binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "demo-step" {
		cmdDemoStep(os.Args[2:])
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRunLiveStreamsLinesAndExitCode(t *testing.T) {
	argv := []string{"sh", "-c", "echo one; echo two; exit 3"}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/c", "echo one&echo two&exit 3"}
	}
	var lines []string
	code, out := runLive(argv, nil, 30*time.Second, func(l string) { lines = append(lines, l) })
	if code != 3 {
		t.Errorf("exit code %d, want 3", code)
	}
	if strings.Join(lines, "|") != "one|two" || !strings.Contains(out, "two") {
		t.Errorf("lines=%v out=%q", lines, out)
	}
}

func TestRunLiveTimeout(t *testing.T) {
	argv := []string{"sh", "-c", "sleep 5"}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/c", "ping -n 6 127.0.0.1 >nul"}
	}
	code, out := runLive(argv, nil, 400*time.Millisecond, nil)
	if code != 124 || !strings.Contains(out, "timed out") {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestDemoJobStreamsStepsAndHints(t *testing.T) {
	t.Setenv("OPENITE_DEMO_FAIL", "chrome")
	b := newDemoBackend()
	job := Job{Steps: []Step{
		{Type: "install", Key: "firefox", App: "Firefox", Pkg: "Mozilla.Firefox"},
		{Type: "install", Key: "chrome", App: "Google Chrome", Pkg: "Google.Chrome"},
		{Type: "skip", App: "Teams", Reason: "no winget package id"},
	}}
	var mu sync.Mutex
	var lines []string
	status := map[int]string{}
	hints := map[int]string{}
	res := executeJob(job, b, jobHooks{
		Line: func(l string) { mu.Lock(); lines = append(lines, l); mu.Unlock() },
		Step: func(i int, st, h string) { mu.Lock(); status[i], hints[i] = st, h; mu.Unlock() },
	})
	if res != "failed" {
		t.Fatalf("job result %q, want failed (chrome step is set to fail)", res)
	}
	if status[0] != "done" || status[1] != "failed" || status[2] != "skipped" {
		t.Errorf("step statuses %v", status)
	}
	if !strings.Contains(hints[1], "installer failed") {
		t.Errorf("hint for the failed step: %q", hints[1])
	}
	log := strings.Join(lines, "\n")
	for _, want := range []string{"=== [1/3] install Firefox", "Successfully installed", "Installer failed with exit code: 1603", "hint:"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
	if strings.ContainsAny(log, "█▒") {
		t.Error("progress bars must be filtered out of the log")
	}
	if inv := b.Inventory(nil); inv.Installed["Mozilla.Firefox"] != "latest" {
		t.Errorf("demo machine state not updated after the successful step: %v", inv.Installed)
	}
}
