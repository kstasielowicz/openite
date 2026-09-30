package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Colour output only when talking to a real terminal (and NO_COLOR isn't set), so pipes and logs stay clean.
var (
	interactive = false
	useColor    = false
)

func initTerminal() {
	enableVT()
	interactive = isTerminal(os.Stdin) && isTerminal(os.Stdout)
	useColor = isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == ""
}

func sgr(code, s string) string {
	if !useColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func bold(s string) string   { return sgr("1", s) }
func dim(s string) string    { return sgr("2", s) }
func red(s string) string    { return sgr("31", s) }
func green(s string) string  { return sgr("32", s) }
func yellow(s string) string { return sgr("33", s) }
func cyan(s string) string   { return sgr("36", s) }
func accent(s string) string { return sgr("1;38;5;75", s) }

func banner(sub string) {
	fmt.Printf("\n  %s %s  %s\n", accent("⬇ Openite"), dim("v"+version), dim(sub))
	fmt.Println("  " + dim(strings.Repeat("─", 56)))
}

func okMark() string   { return green("✔") }
func failMark() string { return red("✘") }

// spinner runs fn while animating a one-line spinner with a label; returns fn's result.
func withSpinner(label string, fn func() bool) bool {
	if !interactive {
		fmt.Printf("%s … ", label)
		ok := fn()
		if ok {
			fmt.Println("done")
		} else {
			fmt.Println("FAILED")
		}
		return ok
	}
	done := make(chan bool)
	go func() { done <- fn() }()
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	for i := 0; ; i++ {
		select {
		case ok := <-done:
			mark := okMark()
			if !ok {
				mark = failMark()
			}
			fmt.Printf("\r\x1b[2K  %s %s\n", mark, label)
			return ok
		default:
			fmt.Printf("\r\x1b[2K  %s %s", cyan(frames[i%len(frames)]), label)
			sleepMs(80)
		}
	}
}

func sleepMs(n int) { time.Sleep(time.Duration(n) * time.Millisecond) }
