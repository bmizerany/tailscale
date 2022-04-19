// Copyright (c) 2022 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ping

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"inet.af/netaddr"
)

func TestParseReplyCommand(t *testing.T) {
	var check = func(addr string) {
		b, err := Command(netaddr.MustParseIP(addr)).CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		dur, ip, err := ParseReply(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if ip != netaddr.MustParseIP(addr) {
			t.Errorf("got %s, wanted %q", ip, addr)
		}
		if dur == 0 {
			t.Errorf("got %s, wanted >0 duration", dur)
		}
	}

	t.Run("v4", func(t *testing.T) { check("127.0.0.1") })
	t.Run("v6", func(t *testing.T) { check("::1") })
}

func TestParseWindows(t *testing.T) {
	var examples = map[string]string{
		"local": `
Pinging 127.0.0.1 with 32 bytes of data:
Reply from 127.0.0.1: bytes=32 time<1ms TTL=128

Ping statistics for 127.0.0.1:
    Packets: Sent = 1, Received = 1, Lost = 0 (0% loss),
Approximate round trip times in milli-seconds:
    Minimum = 0ms, Maximum = 0ms, Average = 0ms
`,

		"local v6": `
Pinging ::1 with 32 bytes of data:
Reply from ::1: time<1ms

Ping statistics for ::1:
    Packets: Sent = 1, Received = 1, Lost = 0 (0% loss),
Approximate round trip times in milli-seconds:
    Minimum = 0ms, Maximum = 0ms, Average = 0ms
`,

		"google v6": `
Pinging 2001:4860:4860::8888 with 32 bytes of data:
Reply from 2001:4860:4860::8888: time=12ms

Ping statistics for 2001:4860:4860::8888:
    Packets: Sent = 1, Received = 1, Lost = 0 (0% loss),
Approximate round trip times in milli-seconds:
    Minimum = 12ms, Maximum = 12ms, Average = 12ms
	`,

		"slow": `
Pinging 192.168.0.1 with 32 bytes of data:
Reply from 192.168.0.1: bytes=32 time=2525ms TTL=64

Ping statistics for 192.168.0.1:
    Packets: Sent = 1, Received = 1, Lost = 0 (0% loss),
Approximate round trip times in milli-seconds:
    Minimum = 2525ms, Maximum = 2525ms, Average = 2525ms
`,
	}

	var check = func(example, addr string, durEx time.Duration) {
		t.Run(example, func(t *testing.T) {
			dur, ip, err := parseReplyWindows(strings.NewReader(examples[example]))
			if err != nil {
				panic(err)
			}
			if got, want := ip, netaddr.MustParseIP(addr); got != want {
				t.Errorf("ip: got %s, want %s", got, want)
			}
			if got, want := dur, durEx; got != want {
				t.Errorf("got %s, want %s", got, want)
			}

		})
	}

	// round <1ms on windows to 0.5ms, so as to avoid it looking like a 0 value.
	check("local", "127.0.0.1", time.Millisecond/2)
	check("local v6", "::1", time.Millisecond/2)
	check("google v6", "2001:4860:4860::8888", 12*time.Millisecond)
	check("slow", "192.168.0.1", 2525*time.Millisecond)
}

func TestParseUnix(t *testing.T) {
	var examples = map[string]string{
		"local": `
PING 127.0.0.1 (127.0.0.1) 56(84) bytes of data.
64 bytes from 127.0.0.1: icmp_seq=1 ttl=64 time=0.017 ms

--- 127.0.0.1 ping statistics ---
1 packets transmitted, 1 received, 0% packet loss, time 0ms
rtt min/avg/max/mdev = 0.017/0.017/0.017/0.000 ms
`,
		"local v6": `
PING ::1(::1) 56 data bytes
64 bytes from ::1: icmp_seq=1 ttl=64 time=0.016 ms

--- ::1 ping statistics ---
1 packets transmitted, 1 received, 0% packet loss, time 0ms
rtt min/avg/max/mdev = 0.016/0.016/0.016/0.000 ms
`,

		"google v6": `
PING 2001:4860:4860::8888(2001:4860:4860::8888) 56 data bytes
64 bytes from 2001:4860:4860::8888: icmp_seq=1 ttl=118 time=8.89 ms

--- 2001:4860:4860::8888 ping statistics ---
1 packets transmitted, 1 received, 0% packet loss, time 0ms
rtt min/avg/max/mdev = 8.892/8.892/8.892/0.000 ms
`,

		"slow": `
PING 192.168.0.1 (192.168.0.1) 56(84) bytes of data.
64 bytes from 192.168.0.1: icmp_seq=1 ttl=64 time=2500 ms

--- 192.168.0.1 ping statistics ---
1 packets transmitted, 1 received, 0% packet loss, time 0ms
rtt min/avg/max/mdev = 2500.310/2500.310/2500.310/0.000 ms
`,
	}

	var check = func(example, addr string, durEx time.Duration) {
		t.Run(example, func(t *testing.T) {
			dur, ip, err := parseReplyUnix(strings.NewReader(examples[example]))
			if err != nil {
				panic(err)
			}
			if got, want := ip, netaddr.MustParseIP(addr); got != want {
				t.Errorf("ip: got %s, want %s", got, want)
			}
			if got, want := dur, durEx; got != want {
				t.Errorf("got %s, want %s", got, want)
			}
		})
	}

	check("local", "127.0.0.1", 17*time.Microsecond)
	check("local v6", "::1", 16*time.Microsecond)
	check("google v6", "2001:4860:4860::8888", 8890*time.Microsecond)
	check("slow", "192.168.0.1", 2500*time.Millisecond)
}
