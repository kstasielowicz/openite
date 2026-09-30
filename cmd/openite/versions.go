package main

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/openite/openite/catalog"
)

// Available versions of a package, newest first. Read-only; works even in dry-run.

var (
	verMu    sync.Mutex
	verCache = map[string]verEntry{}
)

type verEntry struct {
	at       time.Time
	versions []string
}

func versionsFor(b Backend, a catalog.App) ([]string, error) {
	pkg := a.Pkg(b.Name())
	if pkg == "" || !catalog.SafeID.MatchString(pkg) {
		return nil, fmt.Errorf("%s is not available via %s", a.Name, b.Name())
	}
	key := b.Name() + "/" + pkg
	verMu.Lock()
	if e, ok := verCache[key]; ok && time.Since(e.at) < 10*time.Minute {
		verMu.Unlock()
		return e.versions, nil
	}
	verMu.Unlock()
	var vs []string
	switch b.Name() {
	case "winget":
		rc, out := probe(cat([]string{"winget", "show", "--id", pkg, "-e", "--versions"}, wingetBase), nil, 2*time.Minute)
		if rc != 0 {
			return nil, fmt.Errorf("winget could not list versions for %s", pkg)
		}
		vs = versionList(out)
	case "apt":
		_, out := probe([]string{"apt-cache", "madison", pkg}, nil, time.Minute)
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Split(l, "|"); len(f) >= 2 {
				if v := strings.TrimSpace(f[1]); catalog.SafeVersion.MatchString(v) && !contains(vs, v) {
					vs = append(vs, v)
				}
			}
		}
	default:
		return nil, fmt.Errorf("%s cannot install a specific version", b.Name())
	}
	verMu.Lock()
	verCache[key] = verEntry{time.Now(), vs}
	verMu.Unlock()
	return vs, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

var reVerNum = regexp.MustCompile(`\d+`)

// cmpVersions compares dotted versions numerically ("2.9" < "2.10"); non-numeric parts are ignored. -1, 0, 1.
func cmpVersions(a, b string) int {
	x, y := reVerNum.FindAllString(a, -1), reVerNum.FindAllString(b, -1)
	for i := 0; i < len(x) || i < len(y); i++ {
		var p, q int
		if i < len(x) {
			fmt.Sscan(x[i], &p)
		}
		if i < len(y) {
			fmt.Sscan(y[i], &q)
		}
		if p != q {
			if p < q {
				return -1
			}
			return 1
		}
	}
	return 0
}

// cmdVersions: `openite versions git` lists what can be installed with `openite install git@<version>`.
func cmdVersions(args []string) {
	if len(args) != 1 {
		die("usage: openite versions <app>")
	}
	apps, _, err := resolve(args, nil)
	if err != nil {
		die("%v", err)
	}
	b := needBackend()
	var vs []string
	withSpinner("Looking up versions of "+apps[0].Name, func() bool { vs, err = versionsFor(b, apps[0]); return err == nil })
	if err != nil {
		die("%v", err)
	}
	if len(vs) > 25 {
		fmt.Printf("  %s\n", dim(fmt.Sprintf("(showing 25 of %d, newest first)", len(vs))))
		vs = vs[:25]
	}
	for _, v := range vs {
		fmt.Printf("  %s\n", v)
	}
	fmt.Printf("\n  Install one with: %s\n\n", cyan("openite install "+apps[0].Key+"@"+vs[0]))
}
