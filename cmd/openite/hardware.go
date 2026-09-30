package main

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/openite/openite/catalog"
)

// Driver help. Openite deliberately does NOT download or install raw driver packages (a wrong or tampered
// driver can brick a PC). It (1) detects your hardware, (2) installs the vendor's own update tool when winget
// carries it, and (3) opens the vendor's official download page otherwise. Windows Update is always suggested.

type Hardware struct {
	GPUs  []string `json:"gpus"`
	Maker string   `json:"maker"`
	Model string   `json:"model"`
	CPU   string   `json:"cpu"`
}

type Advice struct {
	Title  string `json:"title"`
	Why    string `json:"why"`
	AppKey string `json:"app_key,omitempty"` // install this catalog app...
	URL    string `json:"url,omitempty"`     // ...or open this official page
}

const psHardware = `$g=@(Get-CimInstance Win32_VideoController | ForEach-Object { $_.Name });` +
	`$s=Get-CimInstance Win32_ComputerSystem;$c=Get-CimInstance Win32_Processor | Select-Object -First 1;` +
	`@{gpus=$g;maker=$s.Manufacturer;model=$s.Model;cpu=$c.Name} | ConvertTo-Json -Compress`

func detectHardware() Hardware {
	var h Hardware
	if runtime.GOOS != "windows" {
		return h
	}
	_, out := probe([]string{"powershell", "-NoProfile", "-NonInteractive", "-Command", psHardware}, nil, time.Minute)
	if json.Unmarshal([]byte(out), &h) != nil {
		// a single GPU makes PowerShell emit a string instead of a one-element array
		var one struct {
			GPUs  string `json:"gpus"`
			Maker string `json:"maker"`
			Model string `json:"model"`
			CPU   string `json:"cpu"`
		}
		if json.Unmarshal([]byte(out), &one) == nil {
			h = Hardware{GPUs: []string{one.GPUs}, Maker: one.Maker, Model: one.Model, CPU: one.CPU}
		}
	}
	return h
}

// driverAdvice maps hardware to official tools/pages. Every URL here is a fixed vendor address.
func driverAdvice(h Hardware) []Advice {
	var out []Advice
	seen := map[string]bool{}
	add := func(a Advice) {
		if !seen[a.Title] {
			seen[a.Title] = true
			out = append(out, a)
		}
	}
	for _, g := range h.GPUs {
		l := strings.ToLower(g)
		switch {
		case strings.Contains(l, "nvidia"):
			add(Advice{Title: "NVIDIA graphics driver", Why: "Detected " + g + ". Get the NVIDIA App / driver from NVIDIA.", URL: "https://www.nvidia.com/en-us/drivers/"})
		case strings.Contains(l, "amd") || strings.Contains(l, "radeon"):
			add(Advice{Title: "AMD graphics driver", Why: "Detected " + g + ". Get AMD Software: Adrenalin Edition from AMD.", URL: "https://www.amd.com/en/support/download/drivers.html"})
		case strings.Contains(l, "intel"):
			add(Advice{Title: "Intel Driver & Support Assistant", Why: "Detected " + g + ". Intel's official tool finds graphics, Wi-Fi and chipset drivers.", AppKey: "intel-dsa"})
		}
	}
	cpu := strings.ToLower(h.CPU)
	if strings.Contains(cpu, "intel") {
		add(Advice{Title: "Intel Driver & Support Assistant", Why: "Intel CPU: finds chipset, Wi-Fi and Bluetooth drivers.", AppKey: "intel-dsa"})
	} else if strings.Contains(cpu, "amd") {
		add(Advice{Title: "AMD chipset driver", Why: "AMD CPU: install the current chipset driver from AMD.", URL: "https://www.amd.com/en/support/download/drivers.html"})
	}
	maker := strings.ToLower(h.Maker)
	switch {
	case strings.Contains(maker, "dell"):
		add(Advice{Title: "Dell Command | Update", Why: "Dell PC (" + h.Model + "): Dell's own BIOS/driver updater.", AppKey: "dell-command-update"})
	case strings.Contains(maker, "lenovo"):
		add(Advice{Title: "Lenovo System Update", Why: "Lenovo PC (" + h.Model + "): Lenovo's own driver updater.", AppKey: "lenovo-system-update"})
	case strings.Contains(maker, "hp") || strings.Contains(maker, "hewlett"):
		add(Advice{Title: "HP Support Assistant", Why: "HP PC (" + h.Model + "): HP's own driver updater.", URL: "https://support.hp.com/us-en/help/hp-support-assistant"})
	case strings.Contains(maker, "asus"):
		add(Advice{Title: "ASUS driver downloads", Why: "ASUS PC (" + h.Model + ").", URL: "https://www.asus.com/support/download-center/"})
	case strings.Contains(maker, "micro-star") || strings.Contains(maker, "msi"):
		add(Advice{Title: "MSI driver downloads", Why: "MSI PC (" + h.Model + ").", URL: "https://www.msi.com/support/download"})
	case strings.Contains(maker, "gigabyte"):
		add(Advice{Title: "Gigabyte driver downloads", Why: "Gigabyte PC (" + h.Model + ").", URL: "https://www.gigabyte.com/Support"})
	}
	add(Advice{Title: "Windows Update", Why: "Delivers many drivers (audio, network, touchpad…) automatically. Always worth running first.", URL: "ms-settings:windowsupdate"})
	return out
}

func cmdDrivers([]string) {
	if runtime.GOOS != "windows" {
		die("`openite drivers` is for Windows. On Linux use your distribution's driver manager (e.g. `ubuntu-drivers`).")
	}
	b := needBackend()
	banner("drivers & hardware")
	var h Hardware
	withSpinner("Detecting your hardware", func() bool { h = detectHardware(); return true })
	fmt.Printf("\n  %s %s %s\n  %s %s\n  %s %s\n\n", bold("PC:"), h.Maker, h.Model, bold("CPU:"), h.CPU, bold("GPU:"), strings.Join(h.GPUs, ", "))
	fmt.Println("  " + dim("Openite never installs raw driver packages; it installs each vendor's own update tool or opens the"))
	fmt.Println("  " + dim("official download page, so the vendor's installer does the risky part."))
	for _, a := range driverAdvice(h) {
		fmt.Printf("\n  %s %s\n    %s\n", cyan("→"), bold(a.Title), dim(a.Why))
		switch {
		case a.AppKey != "":
			app, _ := catalog.Find(a.AppKey)
			if confirm("    Install " + app.Name + " now?") {
				runChange("install", []catalog.App{app}, nil, b, true)
			}
		case a.URL != "":
			q := "    Open the official page in your browser?"
			if strings.HasPrefix(a.URL, "ms-settings:") {
				q = "    Open Windows Update?"
			}
			if confirm(q) {
				openBrowser(a.URL)
			}
		}
	}
	fmt.Println()
}
