package main

import "testing"

func TestCheckServerURL(t *testing.T) {
	ok := []string{"https://example.com", "http://192.168.1.10:8080", "http://10.0.0.5", "http://localhost:8080", "http://127.0.0.1:1", "http://nas:8080", "http://box.lan", "http://[::1]:8080"}
	bad := []string{"http://example.com", "http://8.8.8.8", "ftp://x", "nonsense", "", "http://evil.com:8080"}
	for _, u := range ok {
		if err := checkServerURL(u, false); err != nil {
			t.Errorf("%s should be allowed: %v", u, err)
		}
	}
	for _, u := range bad {
		if checkServerURL(u, false) == nil {
			t.Errorf("%s should be refused", u)
		}
	}
	if checkServerURL("http://example.com", true) != nil {
		t.Error("--allow-insecure must override")
	}
}
