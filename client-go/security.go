package main

import (
	"context"
	"net"
	"net/url"
	"strings"
	"time"
)

// lookupIPWithTimeout resolves hostnames with a 2s timeout. Seam for tests.
var lookupIPWithTimeout = func(host string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

var allowLoopbackDiscovery = false

// IsAdoptableIP reports whether ip is a private LAN unicast address (RFC 1918 or link-local unicast).
// Loopback (127.0.0.0/8, ::1), unspecified (0.0.0.0, ::), and multicast are explicitly rejected (spec §6).
func IsAdoptableIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return allowLoopbackDiscovery
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalMulticast() {
		return false
	}
	return ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// IsPrivateIP reports whether ip is loopback, link-local or RFC1918 private.
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate()
}

// AdoptableServerURL reports whether a server URL advertised via discovery
// is safe to auto-adopt. Only http(s) URLs pointing at private LAN unicast addresses
// are accepted. Hostnames must resolve within 2s and ALL resolved addresses must be adoptable private IPs.
// Loopback is never adopted from the network (spec §6).
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
		addrs, err := lookupIPWithTimeout(host)
		if err != nil || len(addrs) == 0 {
			return false
		}
		for _, a := range addrs {
			if !IsAdoptableIP(a) {
				return false
			}
		}
		return true
	}
	return IsAdoptableIP(ip)
}

// VerifySHA256 reports whether dataHasher's digest matches the expected
// lowercase/uppercase hex string. An empty expected value fails closed.
func VerifySHA256(actualHex, expectedHex string) bool {
	if expectedHex == "" {
		return false
	}
	return strings.EqualFold(actualHex, expectedHex)
}
