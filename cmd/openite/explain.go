package main

import (
	"regexp"
	"strings"
)

// Failure hints: turn "exit 1603" into something a person can act on. Best-effort pattern matching on the package
// manager's exit code and output; a hint is advice, never a claim about the exact cause. First match wins.

type hintRule struct {
	codes []int
	re    *regexp.Regexp
	hint  string
}

func rule(codes []int, pattern, hint string) hintRule {
	var re *regexp.Regexp
	if pattern != "" {
		re = regexp.MustCompile(`(?i)` + pattern)
	}
	return hintRule{codes, re, hint}
}

var hintRules = []hintRule{
	rule(nil, `refusing unsafe`, "Openite refused a package id or version that looks unsafe. This is a safety check; review the app definition."),
	rule(nil, `cannot install a specific version`, "Homebrew can only install the latest version. Install without a version, or use Hold to stop updates."),
	rule([]int{740}, `requires? (admin|elevation)|administrator (rights|privileges)|run as administrator|elevated|exit code: 740|access is denied|0x80070005|\(os error 5\)`,
		"This needs administrator rights. Close Openite and start it again with \"Run as administrator\" (agents: run the service from an elevated terminal)."),
	rule([]int{1602, 1223}, `cancel+ed by (the )?user|user cancel`, "The installer was cancelled before it finished. Run it again and let it complete."),
	rule([]int{1618}, `another installation is (already )?in progress|1618`, "Windows is already installing something else. Wait for it to finish, then try again."),
	rule([]int{1638}, `another version of this product is already installed`, "A different version is already installed. Use Update instead, or uninstall it first."),
	rule([]int{1603}, `exit code: 1603|fatal error during installation`, "The app's own installer failed (error 1603). Close the app if it is open, restart Windows and try again. The lines above often hold the vendor's reason."),
	rule(nil, `hash does not match|installer hash|hash mismatch`, "The download didn't match its checksum, so winget refused it. Try again in a few minutes; the package may be mid-update."),
	rule(nil, `no package found matching|no applicable installer|no applicable upgrade`, "winget can't find this package for your system. It may not exist for your CPU or region, or the source needs a refresh (winget source update)."),
	rule(nil, `could not resolve|remote name could not be resolved|connection (was )?(reset|closed|refused)|failed when opening source|0x80072ee7|0x80072efd|0x80072f8f|network (is )?unreachable|timed out while|temporary failure in name resolution`,
		"Can't reach the internet or the package source. Check your connection or proxy, then try again."),
	rule(nil, `could not get lock|unable to acquire the dpkg|dpkg frontend lock`, "Another package manager is running. Wait a minute, then retry."),
	rule(nil, `unable to locate package|e: package .* has no installation candidate`, "apt can't find this package. Run \"sudo apt update\" first, or enable the repository that provides it."),
	rule(nil, `sudo: (a password is required|a terminal is required|no tty present)`, "The agent needs passwordless sudo for package commands, or must run as root."),
	rule([]int{124}, `timed out after`, "It ran longer than the time limit and was stopped. Try again; large installs may need a faster connection."),
	rule([]int{127}, `command not found`, "The package manager isn't installed or isn't on PATH for this account."),
}

// explainFailure returns a short plain-language hint for a failed step ("" when nothing useful can be said).
func explainFailure(code int, output string) string {
	for _, r := range hintRules {
		matched := false
		for _, c := range r.codes {
			if c == code {
				matched = true
			}
		}
		if !matched && r.re != nil && r.re.MatchString(output) {
			matched = true
		}
		if matched {
			return r.hint
		}
	}
	if strings.TrimSpace(output) == "" {
		return ""
	}
	return "Something went wrong. The log above has the package manager's own message."
}
