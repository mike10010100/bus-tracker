package main

import (
	"net"
	"net/url"
	"strings"
)

// AdoptableServerURL reports whether a server URL advertised via the
// X-Tracker-Server header (or discovery offer) is safe to auto-adopt.
// Only http(s) URLs pointing at a loopback, link-local or RFC1918 private
// address are accepted, so a hostile public host cannot hijack the client's
// OTA channel simply by returning a header.
func AdoptableServerURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Allow bare LAN hostnames only if they resolve to a private address.
		addrs, err := net.LookupIP(host)
		if err != nil {
			return false
		}
		for _, a := range addrs {
			if IsPrivateIP(a) {
				return true
			}
		}
		return false
	}
	return IsPrivateIP(ip)
}

// IsPrivateIP reports whether ip is loopback, link-local or RFC1918 private.
func IsPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate()
}

// VerifySHA256 reports whether dataHasher's digest matches the expected
// lowercase/uppercase hex string. An empty expected value fails closed.
func VerifySHA256(actualHex, expectedHex string) bool {
	if expectedHex == "" {
		return false
	}
	return strings.EqualFold(actualHex, expectedHex)
}
