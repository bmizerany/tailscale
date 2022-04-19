// Copyright (c) 2022 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package controlclient

import (
	"bytes"
	"fmt"
	"os/exec"

	"inet.af/netaddr"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/ping"
)

// ICMPPinger wraps the ping package in a Pinger interface.
type ICMPPinger struct{}

func (i *ICMPPinger) Ping(ip netaddr.IP, useTSMP bool, cb func(*ipnstate.PingResult)) {
	var pr ipnstate.PingResult
	pr.IP = ip.String()

	b, err := ping.Command(ip).Output()
	if err != nil {

		if exerr, ok := err.(*exec.ExitError); ok {
			pr.Err = fmt.Sprintf("exit code: %d\n%s", exerr.ExitCode(), exerr.Stderr)
		} else {
			pr.Err = err.Error()
		}
	}

	dur, ip, err := ping.ParseReply(bytes.NewReader(b))
	if err != nil {
		pr.Err = err.Error()
	} else {
		pr.Endpoint = ip.String()
		pr.LatencySeconds = dur.Seconds()
	}

	cb(&pr)
}
