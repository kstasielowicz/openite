package main

import (
	"encoding/json"
	"net"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SysInfo is what a device tells the UI about itself: enough to recognise a machine and judge its health,
// nothing sensitive (no serial numbers, no user names beyond the account running the agent, no file contents).
type SysInfo struct {
	Hostname   string   `json:"hostname"`
	OS         string   `json:"os"`
	OSVersion  string   `json:"os_version"`
	Arch       string   `json:"arch"`
	Kernel     string   `json:"kernel,omitempty"`
	CPU        string   `json:"cpu"`
	Cores      int      `json:"cores"`
	Threads    int      `json:"threads"`
	MemTotalMB int64    `json:"mem_total_mb"`
	MemFreeMB  int64    `json:"mem_free_mb"`
	DiskTotal  int64    `json:"disk_total_gb"`
	DiskFree   int64    `json:"disk_free_gb"`
	GPUs       []string `json:"gpus"`
	Maker      string   `json:"maker"`
	Model      string   `json:"model"`
	Virtual    string   `json:"virtual,omitempty"` // hypervisor name when this is a VM
	BootUnix   int64    `json:"boot_unix"`
	UptimeSec  int64    `json:"uptime_s"`
	IPs        []string `json:"ips"`
	Timezone   string   `json:"timezone"`
	User       string   `json:"user"`
	Elevated   bool     `json:"elevated"`
}

type flexStrings []string

// PowerShell emits a bare string for one element and an array for several.
func (f *flexStrings) UnmarshalJSON(b []byte) error {
	var many []string
	if json.Unmarshal(b, &many) == nil {
		*f = many
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err != nil {
		return err
	}
	*f = []string{one}
	return nil
}

var (
	sysMu    sync.Mutex
	sysCache SysInfo
	sysAt    time.Time
)

// currentSysInfo is cached for 10 minutes (collecting takes a second or two on Windows); uptime is always fresh.
func currentSysInfo() SysInfo {
	sysMu.Lock()
	defer sysMu.Unlock()
	if sysCache.Hostname == "" || time.Since(sysAt) > 10*time.Minute {
		sysCache, sysAt = collectSysInfo(), time.Now()
	}
	s := sysCache
	if s.BootUnix > 0 {
		s.UptimeSec = time.Now().Unix() - s.BootUnix
	}
	return s
}

const psSysInfo = `$os=Get-CimInstance Win32_OperatingSystem;$cs=Get-CimInstance Win32_ComputerSystem;$cpu=@(Get-CimInstance Win32_Processor);` +
	`$d=Get-CimInstance Win32_LogicalDisk -Filter ("DeviceID='" + $env:SystemDrive + "'");` +
	`[ordered]@{os=$os.Caption;osver=$os.Version;build=$os.BuildNumber;memKB=$os.TotalVisibleMemorySize;freeKB=$os.FreePhysicalMemory;` +
	`boot=[DateTimeOffset]::new($os.LastBootUpTime).ToUnixTimeSeconds();maker=$cs.Manufacturer;model=$cs.Model;cpu=$cpu[0].Name;` +
	`cores=($cpu|Measure-Object NumberOfCores -Sum).Sum;threads=($cpu|Measure-Object NumberOfLogicalProcessors -Sum).Sum;` +
	`diskTotal=$d.Size;diskFree=$d.FreeSpace;gpus=@(Get-CimInstance Win32_VideoController|ForEach-Object{$_.Name});tz=(Get-TimeZone).Id;` +
	`elev=([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)}|ConvertTo-Json -Compress`

var vmHints = []string{"virtual", "vmware", "kvm", "qemu", "hvm domu", "xen", "bochs", "parallels", "amazon ec2", "google compute", "openstack"}

func vmFromModel(maker, model string) string {
	l := strings.ToLower(maker + " " + model)
	for _, h := range vmHints {
		if strings.Contains(l, h) {
			return strings.TrimSpace(maker + " " + model)
		}
	}
	return ""
}

func localIPs() []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, in := range ifs {
		if in.Flags&net.FlagUp == 0 || in.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := in.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip := ipn.IP.To4(); ip != nil && !ip.IsLinkLocalUnicast() {
					out = append(out, ip.String())
				}
			}
		}
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

func collectSysInfo() SysInfo {
	host, _ := os.Hostname()
	s := SysInfo{Hostname: host, Arch: runtime.GOARCH, Threads: runtime.NumCPU(), IPs: localIPs(), GPUs: []string{}, User: os.Getenv("USER")}
	switch runtime.GOOS {
	case "windows":
		s.User = os.Getenv("USERNAME")
		_, out := probe([]string{"powershell", "-NoProfile", "-NonInteractive", "-Command", psSysInfo}, nil, time.Minute)
		var w struct {
			OS, OSVer, Build, Maker, Model, CPU, TZ string
			MemKB, FreeKB, Boot, Cores, Threads     int64
			DiskTotal, DiskFree                     int64
			GPUs                                    flexStrings `json:"gpus"`
			Elev                                    bool
		}
		if json.Unmarshal([]byte(out), &w) == nil {
			s.OS, s.OSVersion = strings.TrimSpace(w.OS), w.OSVer+" (build "+w.Build+")"
			s.Maker, s.Model, s.CPU, s.Timezone = strings.TrimSpace(w.Maker), strings.TrimSpace(w.Model), strings.TrimSpace(w.CPU), w.TZ
			s.MemTotalMB, s.MemFreeMB = w.MemKB/1024, w.FreeKB/1024
			s.DiskTotal, s.DiskFree = w.DiskTotal>>30, w.DiskFree>>30
			s.Cores, s.BootUnix, s.GPUs, s.Elevated = int(w.Cores), w.Boot, w.GPUs, w.Elev
			if w.Threads > 0 {
				s.Threads = int(w.Threads)
			}
			s.Virtual = vmFromModel(s.Maker, s.Model)
		}
	case "linux":
		s.OS, s.OSVersion = "Linux", ""
		if b, err := os.ReadFile("/etc/os-release"); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(l, "PRETTY_NAME=") {
					s.OS = strings.Trim(strings.TrimPrefix(l, "PRETTY_NAME="), `"`)
				}
			}
		}
		_, k := probe([]string{"uname", "-r"}, nil, 5*time.Second)
		s.Kernel = strings.TrimSpace(k)
		s.OSVersion = s.Kernel
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(l, "model name") {
					s.CPU = strings.TrimSpace(l[strings.Index(l, ":")+1:])
					break
				}
			}
		}
		if b, err := os.ReadFile("/proc/meminfo"); err == nil {
			s.MemTotalMB, s.MemFreeMB = meminfoMB(string(b), "MemTotal"), meminfoMB(string(b), "MemAvailable")
		}
		if b, err := os.ReadFile("/proc/uptime"); err == nil {
			if f := strings.Fields(string(b)); len(f) > 0 {
				if up, err := strconv.ParseFloat(f[0], 64); err == nil {
					s.BootUnix = time.Now().Unix() - int64(up)
				}
			}
		}
		s.Maker, s.Model = readTrim("/sys/class/dmi/id/sys_vendor"), readTrim("/sys/class/dmi/id/product_name")
		_, v := probe([]string{"systemd-detect-virt"}, nil, 5*time.Second)
		if v = strings.TrimSpace(v); v != "" && v != "none" && !strings.Contains(v, "not found") && !strings.Contains(v, "command") {
			s.Virtual = v
		} else {
			s.Virtual = vmFromModel(s.Maker, s.Model)
		}
		s.Timezone = readTrim("/etc/timezone")
		s.Elevated = os.Geteuid() == 0
		s.DiskTotal, s.DiskFree = dfGB()
	case "darwin":
		_, n := probe([]string{"sw_vers", "-productName"}, nil, 5*time.Second)
		_, v := probe([]string{"sw_vers", "-productVersion"}, nil, 5*time.Second)
		s.OS, s.OSVersion = strings.TrimSpace(n), strings.TrimSpace(v)
		s.CPU = sysctl("machdep.cpu.brand_string")
		s.Model, s.Maker = sysctl("hw.model"), "Apple"
		if m, err := strconv.ParseInt(sysctl("hw.memsize"), 10, 64); err == nil {
			s.MemTotalMB = m >> 20
		}
		if c, err := strconv.Atoi(sysctl("hw.physicalcpu")); err == nil {
			s.Cores = c
		}
		if m := regexp.MustCompile(`sec = (\d+)`).FindStringSubmatch(sysctl("kern.boottime")); m != nil {
			s.BootUnix, _ = strconv.ParseInt(m[1], 10, 64)
		}
		s.Elevated = os.Geteuid() == 0
		s.DiskTotal, s.DiskFree = dfGB()
	}
	if s.Timezone == "" {
		s.Timezone, _ = time.Now().Zone()
	}
	return s
}

func readTrim(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

func sysctl(key string) string {
	_, out := probe([]string{"sysctl", "-n", key}, nil, 5*time.Second)
	return strings.TrimSpace(out)
}

func meminfoMB(text, key string) int64 {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, key+":") {
			if f := strings.Fields(l); len(f) >= 2 {
				kb, _ := strconv.ParseInt(f[1], 10, 64)
				return kb / 1024
			}
		}
	}
	return 0
}

// dfGB reports total/free GB of the root filesystem via `df -Pk /` (works on Linux and macOS).
func dfGB() (total, free int64) {
	_, out := probe([]string{"df", "-Pk", "/"}, nil, 5*time.Second)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) >= 2 {
		if f := strings.Fields(lines[len(lines)-1]); len(f) >= 4 {
			t, _ := strconv.ParseInt(f[1], 10, 64)
			a, _ := strconv.ParseInt(f[3], 10, 64)
			return t >> 20, a >> 20
		}
	}
	return 0, 0
}
