package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// checkServerURL refuses to send enrollment codes and device tokens over plain HTTP to public
// addresses. HTTP is fine on a LAN/loopback (typical home lab); anything else needs https://.
func checkServerURL(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("server must look like https://host:port or http://192.168.1.10:8080")
	}
	if u.Scheme == "https" || allowInsecure {
		return nil
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return nil
		}
	} else {
		h := strings.ToLower(host)
		if !strings.Contains(h, ".") || h == "localhost" {
			return nil // single-label LAN names like "nas"
		}
		for _, suf := range []string{".local", ".lan", ".home", ".internal", ".localdomain"} {
			if strings.HasSuffix(h, suf) {
				return nil
			}
		}
	}
	return fmt.Errorf("refusing plain http:// to %q: tokens and install scripts would travel unencrypted. Use https:// (or --allow-insecure if you really mean it)", host)
}
