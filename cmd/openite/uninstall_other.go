//go:build !windows

package main

import "time"

func listARP() []arpEntry { return nil }

func runRawCmdline(string, time.Duration) (int, string) {
	return 1, "registry uninstall is Windows-only"
}
