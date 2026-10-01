package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// runLive is run() with live output: every line the command prints is handed to onLine as it appears, so the UI can
// show installs while they happen. The returned string is the whole (noise-filtered) output, for failure hints.

// noisy reports package-manager chatter that is useless in a log: spinners and download progress bars.
func noisy(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return true
	}
	if strings.Trim(t, `-\|/ `) == "" { // winget's text spinner
		return true
	}
	return strings.ContainsAny(t, "█▒░▓") // progress bars
}

// splitProgress splits on \n and \r (progress output redraws with \r), dropping empty pieces.
func splitProgress(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func dryPause() {
	if ms, err := strconv.Atoi(os.Getenv("OPENITE_DRY_DELAY_MS")); err == nil && ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
}

func runLive(argv []string, env []string, timeout time.Duration, onLine func(string)) (int, string) {
	if onLine == nil {
		onLine = func(string) {}
	}
	if dryRun {
		line := "[dry-run] " + strings.Join(argv, " ")
		onLine(line)
		dryPause()
		return 0, line
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	r, w, err := os.Pipe()
	if err != nil {
		return -1, err.Error()
	}
	cmd.Stdout, cmd.Stderr = w, w // one merged stream, in the order the command wrote it
	if err := cmd.Start(); err != nil {
		w.Close()
		r.Close()
		if errors.Is(err, exec.ErrNotFound) {
			return 127, "command not found: " + argv[0]
		}
		return -1, err.Error()
	}
	w.Close() // the child keeps its copy; EOF arrives when it exits

	var out strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		sc.Split(splitProgress)
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), " \t")
			if noisy(line) {
				continue
			}
			if out.Len() < 1<<20 {
				out.WriteString(line + "\n")
			}
			onLine(line)
		}
		io.Copy(io.Discard, r)
	}()
	err = cmd.Wait()
	<-done
	r.Close()
	if err == nil {
		return 0, out.String()
	}
	if ctx.Err() == context.DeadlineExceeded {
		msg := fmt.Sprintf("timed out after %s", timeout)
		onLine(msg)
		return 124, out.String() + msg
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), out.String()
	}
	return -1, out.String() + err.Error()
}
