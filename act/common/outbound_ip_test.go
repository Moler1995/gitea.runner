// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package common

import (
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPreferredIP(t *testing.T) {
	assert.Nil(t, preferredIP(nil))
	assert.Equal(t, net.ParseIP("2001:db8::1"), preferredIP([]interfaceIP{{IP: net.ParseIP("2001:db8::1")}}))
	assert.Equal(t, net.ParseIP("2001:db8::1"), preferredIP([]interfaceIP{
		{IP: net.ParseIP("192.0.2.1"), Name: "wlan0"},
		{IP: net.ParseIP("2001:db8::1"), Name: "eth0"},
	}))
}

func TestProbeOutboundIP(t *testing.T) {
	ip := net.IPv4(192, 0, 2, 10)
	for _, tc := range []struct {
		name        string
		wait        time.Duration
		failures    int
		wantIP      net.IP
		wantDials   []time.Duration
		wantElapsed time.Duration
	}{
		{
			name:      "route ready",
			wait:      10 * time.Second,
			wantIP:    ip,
			wantDials: []time.Duration{2 * time.Second},
		},
		{
			name:      "zero budget",
			failures:  1,
			wantDials: []time.Duration{0},
		},
		{
			name:        "route appears within budget",
			wait:        10 * time.Second,
			failures:    2,
			wantIP:      ip,
			wantDials:   []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second},
			wantElapsed: 5 * time.Second,
		},
		{
			name:        "wait capped to budget",
			wait:        2250 * time.Millisecond,
			failures:    1,
			wantDials:   []time.Duration{2 * time.Second},
			wantElapsed: 2250 * time.Millisecond,
		},
		{
			name:        "dial capped to budget",
			wait:        3 * time.Second,
			failures:    2,
			wantDials:   []time.Duration{2 * time.Second, 500 * time.Millisecond},
			wantElapsed: 3 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var dials []time.Duration
				got := probeOutboundIP(tc.wait, func(timeout time.Duration) net.IP {
					dials = append(dials, timeout)
					if len(dials) <= tc.failures {
						time.Sleep(timeout)
						return nil
					}
					return ip
				})
				assert.Equal(t, tc.wantIP, got)
				assert.Equal(t, tc.wantDials, dials)
				assert.Equal(t, tc.wantElapsed, time.Since(start))
			})
		})
	}
}
