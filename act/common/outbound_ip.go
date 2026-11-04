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
	//
	// The dial below needs an IPv4 default route. At boot, SLAAC IPv6 can be
	// configured seconds before DHCPv4 finishes, and dropping into the
	// interface fallback in that window makes the runner advertise a global
	// IPv6 address that jobs on default IPv4-only bridge networks can never
	// reach — every artifact or cache upload then fails with a misleading
	// ENOTFOUND (#1379). Retry briefly before falling back: boot races clear
	// within seconds, and genuinely offline hosts only pay this window once
	// per start (and can skip the probe entirely via cache.host).
	ip := outboundIPLocalAddr()
	deadline := time.Now().Add(10 * time.Second)
	for ip == nil && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		ip = outboundIPLocalAddr()
	}
	if ip != nil {
		return ip
	}

	// So the machine cannot reach the IPv4 internet. Pick an IP address from network interfaces.
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

func outboundIPLocalAddr() net.IP {
	conn, err := net.DialTimeout("udp", "8.8.8.8:80", 2*time.Second)
	if err != nil {
		return nil
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP
	}
	return nil
}
