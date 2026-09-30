package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// `openite schedule ...` sets up automatic updates on THIS machine using the OS scheduler
// (Windows Task Scheduler, or cron on Linux/macOS). For fleets, use schedules in the server UI instead.

const taskName = "Openite Auto Update"
const cronMarker = "# openite-auto-update"

var reTime = regexp.MustCompile(`^([01]?\d|2[0-3]):([0-5]\d)$`)

var days = map[string][2]string{ // name -> {schtasks code, cron number}
	"mon": {"MON", "1"}, "tue": {"TUE", "2"}, "wed": {"WED", "3"}, "thu": {"THU", "4"}, "fri": {"FRI", "5"}, "sat": {"SAT", "6"}, "sun": {"SUN", "0"},
}

const scheduleHelp = `Automatic updates on this PC:
  openite schedule daily 03:00          every day at 03:00
  openite schedule weekly sun 03:00     every Sunday at 03:00
  openite schedule status               show the current schedule
  openite schedule off                  remove it

It runs "openite update -y" (updates everything winget/brew/apt knows about) with your user rights.
On Windows it runs while you are logged in; if the PC was off it does not catch up.
`

func cmdSchedule(args []string) {
	if len(args) == 0 {
		fmt.Print(scheduleHelp)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		die("cannot find own executable: %v", err)
	}
	switch args[0] {
	case "off", "remove", "disable":
		scheduleRemove()
		fmt.Println("Automatic updates turned off.")
	case "status":
		scheduleStatus()
	case "daily", "weekly":
		day, at := "", "03:00"
		rest := args[1:]
		if args[0] == "weekly" {
			if len(rest) == 0 || days[strings.ToLower(rest[0])] == ([2]string{}) {
				die("usage: openite schedule weekly <mon|tue|wed|thu|fri|sat|sun> [HH:MM]")
			}
			day, rest = strings.ToLower(rest[0]), rest[1:]
		}
		if len(rest) > 0 {
			at = rest[0]
		}
		m := reTime.FindStringSubmatch(at)
		if m == nil {
			die("time must look like 03:00 (24-hour)")
		}
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		scheduleInstall(exe, day, h, mi)
	default:
		die("unknown schedule command %q\n\n%s", args[0], scheduleHelp)
	}
}

func scheduleInstall(exe, day string, hour, minute int) {
	hhmm := fmt.Sprintf("%02d:%02d", hour, minute)
	if runtime.GOOS == "windows" {
		tr := fmt.Sprintf(`"%s" update -y`, exe)
		a := []string{"/Create", "/TN", taskName, "/TR", tr, "/ST", hhmm, "/F", "/RL", "HIGHEST"}
		if day == "" {
			a = append(a, "/SC", "DAILY")
		} else {
			a = append(a, "/SC", "WEEKLY", "/D", days[day][0])
		}
		if !sh("schtasks", a...) {
			die("Could not create the scheduled task. Try from an Administrator terminal.")
		}
	} else {
		dow := "*"
		if day != "" {
			dow = days[day][1]
		}
		lines := cronLines()
		lines = append(lines, fmt.Sprintf("%d %d * * %s %s update -y %s", minute, hour, dow, exe, cronMarker))
		writeCron(lines)
	}
	when := "every day"
	if day != "" {
		when = "every " + day
	}
	fmt.Printf("%s Updates will run %s at %s.\n", okMark(), when, hhmm)
}

func scheduleRemove() {
	if runtime.GOOS == "windows" {
		sh("schtasks", "/Delete", "/TN", taskName, "/F")
		return
	}
	writeCron(cronLines())
}

func scheduleStatus() {
	if runtime.GOOS == "windows" {
		if !sh("schtasks", "/Query", "/TN", taskName) {
			fmt.Println("No automatic update schedule is set. Try: openite schedule daily 03:00")
		}
		return
	}
	found := false
	out, _ := exec.Command("crontab", "-l").Output()
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, cronMarker) {
			fmt.Println(l)
			found = true
		}
	}
	if !found {
		fmt.Println("No automatic update schedule is set. Try: openite schedule daily 03:00")
	}
}

// cronLines returns the user's crontab without our own entry.
func cronLines() []string {
	out, _ := exec.Command("crontab", "-l").Output()
	var keep []string
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if l != "" && !strings.Contains(l, cronMarker) {
			keep = append(keep, l)
		}
	}
	return keep
}

func writeCron(lines []string) {
	cmd := exec.Command("crontab", "-")
	cmd.Stdin = bytes.NewBufferString(strings.Join(lines, "\n") + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		die("could not update crontab: %v %s", err, out)
	}
}
