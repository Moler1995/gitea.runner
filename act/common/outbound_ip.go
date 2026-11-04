// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2021 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package common

import (
	"net"
	"sort"
	"strings"
	"time"
)

type interfaceIP struct {
	net.IP
	Name string
}

// GetOutboundIP returns an outbound IP address of this machine.
// It tries to access the internet and returns the local IP address of the connection.
// If the machine cannot access the internet, it returns a preferred IP address from network interfaces.
// It returns nil if no IP address is found.
func GetOutboundIP() net.IP {
	// See https://stackoverflow.com/a/37382208
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err == nil {
		defer conn.Close()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			return addr.IP
		}
	}
	return outboundIPFromInterfaces()
}

// GetOutboundIPWithRetry is GetOutboundIP with a bounded retry budget for the
// UDP probe. At boot, SLAAC IPv6 can be configured seconds before DHCPv4
// finishes, and the interface fallback in that window makes the runner
// advertise a global IPv6 address that jobs on IPv4-only container networks
// can never reach, breaking every artifact or cache upload with ENOTFOUND
// (#1379). Callers that can hit that boot race pass a budget; the probe then
// shares its remaining time between dialing and waiting, and falls back only
// once the budget is spent. If IPv4 appears after the budget the fallback
// address still applies until restart — the lazy re-evaluation alternative is
// tracked in the issue.
func GetOutboundIPWithRetry(wait time.Duration) net.IP {
	if ip := probeOutboundIP(wait); ip != nil {
		return ip
	}
	return outboundIPFromInterfaces()
}

func outboundIPFromInterfaces() net.IP {
	if ifs, err := net.Interfaces(); err == nil {
		var ips []interfaceIP
		for _, i := range ifs {
			if addrs, err := i.Addrs(); err == nil {
				for _, addr := range addrs {
					var ip net.IP
					switch v := addr.(type) {
					case *net.IPNet:
						ip = v.IP
					case *net.IPAddr:
						ip = v.IP
					}
					if ip.IsGlobalUnicast() {
						ips = append(ips, interfaceIP{ip, i.Name})
					}
				}
			}
		}
		return preferredIP(ips)
	}
	return nil
}

func preferredIP(ips []interfaceIP) net.IP {
	if len(ips) == 0 {
		return nil
	}
	sort.Slice(ips, func(i, j int) bool {
		first, second := ips[i], ips[j]

		// ethernet is preferred
		if vi, vj := strings.HasPrefix(first.Name, "e"), strings.HasPrefix(second.Name, "e"); vi != vj {
			return vi
		}

		// IPv4 is preferred
		if vi, vj := first.To4() != nil, second.To4() != nil; vi != vj {
			return vi
		}

		// en0 is preferred to en1
		if first.Name != second.Name {
			return first.Name < second.Name
		}

		// fallback
		return first.String() < second.String()
	})
	return ips[0].IP
}

// Injection points for tests.
var (
	outboundNow          = time.Now
	outboundSleep        = time.Sleep
	outboundDialTimeout  = net.DialTimeout
	outboundDialInterval = 500 * time.Millisecond
	outboundDialBudget   = 2 * time.Second
)

// probeOutboundIP runs the UDP probe until it returns an address or the
// budget is spent. The deadline is taken before the first dial, and each
// dial's own timeout plus the sleep between dials never exceed the remaining
// budget, so the whole probe stays within wait.
func probeOutboundIP(wait time.Duration) net.IP {
	if wait <= 0 {
		if ip := outboundIPLocalAddr(0); ip != nil {
			return ip
		}
		return nil
	}
	deadline := outboundNow().Add(wait)
	for {
		remaining := deadline.Sub(outboundNow())
		if remaining <= 0 {
			return nil
		}
		timeout := outboundDialBudget
		if remaining < timeout {
			timeout = remaining
		}
		if ip := outboundIPLocalAddr(timeout); ip != nil {
			return ip
		}
		remaining = deadline.Sub(outboundNow())
		if remaining <= 0 {
			return nil
		}
		interval := outboundDialInterval
		if remaining < interval {
			interval = remaining
		}
		outboundSleep(interval)
	}
}

func outboundIPLocalAddr(timeout time.Duration) net.IP {
	conn, err := outboundDialTimeout("udp", "8.8.8.8:80", timeout)
	if err != nil {
		return nil
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP
	}
	return nil
}
