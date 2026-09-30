package main

import (
	"regexp"
	"testing"
)

func adviceTitles(h Hardware) map[string]Advice {
	m := map[string]Advice{}
	for _, a := range driverAdvice(h) {
		m[a.Title] = a
	}
	return m
}

func TestDriverAdviceByVendor(t *testing.T) {
	a := adviceTitles(Hardware{GPUs: []string{"NVIDIA GeForce RTX 4070"}, Maker: "Dell Inc.", Model: "XPS", CPU: "Intel(R) Core(TM) i7"})
	for _, want := range []string{"NVIDIA graphics driver", "Intel Driver & Support Assistant", "Dell Command | Update", "Windows Update"} {
		if _, ok := a[want]; !ok {
			t.Errorf("missing advice %q (got %v)", want, a)
		}
	}
	if a["Dell Command | Update"].AppKey != "dell-command-update" {
		t.Error("Dell advice should install the catalog app")
	}
	b := adviceTitles(Hardware{GPUs: []string{"Radeon (TM) RX 480 Graphics"}, CPU: "AMD Ryzen 5 5600X"})
	if _, ok := b["AMD graphics driver"]; !ok {
		t.Error("expected AMD graphics advice")
	}
}

func TestDriverAdviceURLsAreFixedAndSafe(t *testing.T) {
	ok := regexp.MustCompile(`^(https://[a-z0-9.-]+\.(com|org)/[A-Za-z0-9/._-]*|ms-settings:windowsupdate)$`)
	for _, h := range []Hardware{
		{GPUs: []string{"NVIDIA x", "Radeon y", "Intel Arc"}, Maker: "ASUSTeK", CPU: "AMD"},
		{Maker: "HP"}, {Maker: "Micro-Star"}, {Maker: "Gigabyte"}, {Maker: "LENOVO"},
	} {
		for _, a := range driverAdvice(h) {
			if a.URL != "" && !ok.MatchString(a.URL) {
				t.Errorf("unexpected URL in advice %q: %s", a.Title, a.URL)
			}
		}
	}
}

func TestScheduleTimeValidation(t *testing.T) {
	for _, good := range []string{"03:00", "3:05", "23:59", "00:00"} {
		if !reTime.MatchString(good) {
			t.Errorf("%s should be valid", good)
		}
	}
	for _, bad := range []string{"24:00", "3:5", "noon", "12:60", ""} {
		if reTime.MatchString(bad) {
			t.Errorf("%s should be invalid", bad)
		}
	}
}
