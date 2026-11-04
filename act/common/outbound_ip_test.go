// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package common

import (
	"errors"
	"net"
	"testing"
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

type fakeUDPConn struct {
	ip net.IP
}

func (c *fakeUDPConn) Read([]byte) (int, error)         { return 0, errors.New("unused") }
func (c *fakeUDPConn) Write([]byte) (int, error)        { return 0, errors.New("unused") }
func (c *fakeUDPConn) Close() error                     { return nil }
func (c *fakeUDPConn) LocalAddr() net.Addr              { return &net.UDPAddr{IP: c.ip} }
func (c *fakeUDPConn) RemoteAddr() net.Addr             { return &net.UDPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 80} }
func (c *fakeUDPConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeUDPConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeUDPConn) SetWriteDeadline(time.Time) error { return nil }

// probeFixture replaces the clock, the sleep and the dial of probeOutboundIP.
// Each dial consumes the timeout it was granted from the fake clock, so the
// budget accounting of the probe can be asserted without real waiting.
type probeFixture struct {
	now    time.Time
	start  time.Time
	dials  []time.Duration // timeout granted per dial
	slept  []time.Duration
	fail   int // dials failing before the first success
	nextIP net.IP
}

func newProbeFixture(t *testing.T) *probeFixture {
	t.Helper()
	f := &probeFixture{
		now:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		start:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		nextIP: net.IPv4(192, 0, 2, 10),
	}
	undo := []func(){
		swapVar(&outboundNow, func() time.Time { return f.now }),
		swapVar(&outboundSleep, func(d time.Duration) {
			f.slept = append(f.slept, d)
			f.now = f.now.Add(d)
		}),
		swapVar(&outboundDialTimeout, func(_, _ string, timeout time.Duration) (net.Conn, error) {
			f.dials = append(f.dials, timeout)
			if timeout > 0 {
				f.now = f.now.Add(timeout)
			}
			if len(f.dials) <= f.fail {
				return nil, errors.New("no route")
			}
			return &fakeUDPConn{ip: f.nextIP}, nil
		}),
	}
	t.Cleanup(func() {
		for _, u := range undo {
			u()
		}
	})
	return f
}

func swapVar[T any](dst *T, value T) func() {
	old := *dst
	*dst = value
	return func() { *dst = old }
}

func TestProbeSucceedsWithoutRetry(t *testing.T) {
	f := newProbeFixture(t)
	assert.True(t, probeOutboundIP(10*time.Second).Equal(f.nextIP))
	assert.Len(t, f.dials, 1, "one dial is enough when the route is ready")
	assert.Empty(t, f.slept, "no sleep when the route is ready")
	assert.Equal(t, 2*time.Second, f.dials[0])
}

func TestProbeZeroBudgetDialsOnceWithoutWaiting(t *testing.T) {
	f := newProbeFixture(t)
	assert.True(t, probeOutboundIP(0).Equal(f.nextIP))
	assert.Len(t, f.dials, 1, "zero budget must keep the single-shot behavior")
	assert.Equal(t, time.Duration(0), f.dials[0], "zero budget must not impose a dial timeout")
	assert.Empty(t, f.slept)
}

func TestProbeRecoversWithinBudget(t *testing.T) {
	f := newProbeFixture(t)
	f.fail = 2 // the route appears for the third dial
	assert.True(t, probeOutboundIP(10*time.Second).Equal(f.nextIP))
	assert.Len(t, f.dials, 3, "the route appearing at the third dial must be picked up")
	assert.Len(t, f.slept, 2)
}

func TestProbeExhaustsBudget(t *testing.T) {
	f := newProbeFixture(t)
	f.fail = 1 << 30 // never succeeds
	assert.Nil(t, probeOutboundIP(10*time.Second))
	// 2s dials and 500ms sleeps alternate until the 10s budget is spent:
	// 2+.5+2+.5+2+.5+2+.5 = 10, four dials in total.
	assert.Len(t, f.dials, 4)
	assert.Equal(t, 10*time.Second, f.now.Sub(f.start), "the probe must not overspend its budget")
}

func TestProbeCapsTheLastDialToTheRemainingBudget(t *testing.T) {
	f := newProbeFixture(t)
	f.fail = 1 << 30
	assert.Nil(t, probeOutboundIP(3*time.Second))
	// dial@0 gets the full 2s, sleep .5; only 0.5s remain, so the second
	// dial must be capped to the remainder instead of the full budget.
	assert.Equal(t, []time.Duration{2 * time.Second, 500 * time.Millisecond}, f.dials)
	assert.Equal(t, 3*time.Second, f.now.Sub(f.start), "wait and dial must share the budget")
}
