package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// cmdService installs/removes a start-at-boot entry using only the OS's own tooling:
// a Scheduled Task on Windows, a systemd unit on Linux, a LaunchAgent on macOS.
func cmdService(args []string, install bool) {
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfig(), "config file the service should use")
	name := fs.String("name", "openite-agent", "service name (use a different one per agent instance)")
	system := fs.Bool("system", false, "Windows only: run as SYSTEM at boot instead of as you at logon")
	fs.Parse(args)
	exe, err := os.Executable()
	if err != nil {
		die("cannot locate own executable: %v", err)
	}
	cfg, _ := filepath.Abs(*cfgPath)
	if install {
		if _, err := os.Stat(cfg); err != nil {
			die("No config at %s. Run 'enroll' first.", cfg)
		}
	}
	switch runtime.GOOS {
	case "windows":
		if !install {
			sh("schtasks", "/End", "/TN", *name)
			sh("schtasks", "/Delete", "/TN", *name, "/F")
			return
		}
		tr := fmt.Sprintf(`"%s" run --config "%s"`, exe, cfg)
		a := []string{"/Create", "/TN", *name, "/TR", tr, "/F", "/RL", "HIGHEST"}
		if *system {
			a = append(a, "/SC", "ONSTART", "/RU", "SYSTEM")
		} else {
			a = append(a, "/SC", "ONLOGON")
		}
		if sh("schtasks", a...) {
			sh("schtasks", "/Run", "/TN", *name)
			fmt.Println("Installed. The agent now starts automatically.")
		} else {
			fmt.Println("If this says 'Access is denied', re-run from an Administrator terminal.")
		}
	case "linux":
		unit := "/etc/systemd/system/" + *name + ".service"
		if !install {
			sh("systemctl", "disable", "--now", *name)
			os.Remove(unit)
			sh("systemctl", "daemon-reload")
			return
		}
		body := fmt.Sprintf("[Unit]\nDescription=Openite agent\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nExecStart=%s run --config %s\nRestart=always\nRestartSec=10\n\n[Install]\nWantedBy=multi-user.target\n", exe, cfg)
		if err := os.WriteFile(unit, []byte(body), 0o644); err != nil {
			die("cannot write %s (run with sudo): %v", unit, err)
		}
		sh("systemctl", "daemon-reload")
		if sh("systemctl", "enable", "--now", *name) {
			fmt.Println("Installed. The agent now starts automatically at boot.")
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		plist := filepath.Join(home, "Library", "LaunchAgents", "io.openite."+*name+".plist")
		if !install {
			sh("launchctl", "unload", plist)
			os.Remove(plist)
			return
		}
		body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>io.openite.%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>run</string><string>--config</string><string>%s</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
</dict></plist>
`, *name, exe, cfg)
		os.MkdirAll(filepath.Dir(plist), 0o755)
		if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
			die("cannot write %s: %v", plist, err)
		}
		if sh("launchctl", "load", plist) {
			fmt.Println("Installed. The agent now starts automatically at login.")
		}
	default:
		die("service install is not supported on %s; run 'openite run' yourself", runtime.GOOS)
	}
}

func sh(name string, args ...string) bool {
	out, err := exec.Command(name, args...).CombinedOutput()
	if len(out) > 0 {
		fmt.Print(string(out))
	}
	return err == nil
}
