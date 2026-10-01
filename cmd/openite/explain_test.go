package main

import (
	"strings"
	"testing"
)

func TestExplainFailure(t *testing.T) {
	cases := []struct {
		code int
		out  string
		want string // substring of the hint
	}{
		{1603, "Installer failed with exit code: 1603", "installer failed"},
		{740, "", "administrator"},
		{5, "Access is denied.", "administrator"},
		{1618, "Another installation is already in progress", "already installing"},
		{1, "No package found matching input criteria.", "can't find"},
		{1, "Failed when opening source(s); try the 'source reset' command", "internet"},
		{100, "E: Could not get lock /var/lib/dpkg/lock-frontend", "Another package manager"},
		{100, "E: Unable to locate package nope", "apt can't find"},
		{1, "sudo: a password is required", "passwordless sudo"},
		{124, "timed out after 1h0m0s", "time limit"},
		{127, "command not found: winget", "isn't installed"},
		{1, "refusing unsafe package id \"--x\"", "unsafe"},
		{1, "installer hash does not match", "checksum"},
		{9, "some unknown failure", "Something went wrong"},
	}
	for _, c := range cases {
		got := explainFailure(c.code, c.out)
		if !strings.Contains(strings.ToLower(got), strings.ToLower(c.want)) {
			t.Errorf("code %d %q -> %q, want it to mention %q", c.code, c.out, got, c.want)
		}
	}
	if explainFailure(9, "  ") != "" {
		t.Error("no output and no known code should give no hint")
	}
}

func TestNoisyFilter(t *testing.T) {
	for _, l := range []string{"", "   ", "- ", "\\", "|  /  -", "  ██████████▒▒▒▒  54 MB / 81 MB", "▒▒▒▒"} {
		if !noisy(l) {
			t.Errorf("%q should be filtered", l)
		}
	}
	for _, l := range []string{"Successfully installed", "Found Git [Git.Git] Version 2.50", "Downloading https://x/y.exe"} {
		if noisy(l) {
			t.Errorf("%q should be kept", l)
		}
	}
}

func TestSplitProgressHandlesCarriageReturns(t *testing.T) {
	data := []byte("one\rtwo\r\nthree")
	var got []string
	for len(data) > 0 {
		adv, tok, _ := splitProgress(data, true)
		if adv == 0 {
			break
		}
		if len(tok) > 0 {
			got = append(got, string(tok))
		}
		data = data[adv:]
	}
	if strings.Join(got, "|") != "one|two|three" {
		t.Errorf("got %v", got)
	}
}
