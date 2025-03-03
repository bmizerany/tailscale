// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux || (darwin && !ios)
// +build linux darwin,!ios

// Package tailssh is an SSH server integrated into Tailscale.
package tailssh

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	gossh "github.com/tailscale/golang-x-crypto/ssh"
	"inet.af/netaddr"
	"tailscale.com/envknob"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/logtail/backoff"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tailcfg"
	"tailscale.com/tempfork/gliderlabs/ssh"
	"tailscale.com/types/logger"
)

var (
	debugPolicyFile             = envknob.String("TS_DEBUG_SSH_POLICY_FILE")
	debugIgnoreTailnetSSHPolicy = envknob.Bool("TS_DEBUG_SSH_IGNORE_TAILNET_POLICY")
	sshVerboseLogging           = envknob.Bool("TS_DEBUG_SSH_VLOG")
)

type server struct {
	lb             *ipnlocal.LocalBackend
	logf           logger.Logf
	tailscaledPath string

	pubKeyHTTPClient *http.Client     // or nil for http.DefaultClient
	timeNow          func() time.Time // or nil for time.Now

	// mu protects the following
	mu                      sync.Mutex
	activeSessionByH        map[string]*sshSession      // ssh.SessionID (DH H) => session
	activeSessionBySharedID map[string]*sshSession      // yyymmddThhmmss-XXXXX => session
	fetchPublicKeysCache    map[string]pubKeyCacheEntry // by https URL
}

func (srv *server) now() time.Time {
	if srv.timeNow != nil {
		return srv.timeNow()
	}
	return time.Now()
}

func init() {
	ipnlocal.RegisterNewSSHServer(func(logf logger.Logf, lb *ipnlocal.LocalBackend) (ipnlocal.SSHServer, error) {
		tsd, err := os.Executable()
		if err != nil {
			return nil, err
		}
		srv := &server{
			lb:             lb,
			logf:           logf,
			tailscaledPath: tsd,
		}
		return srv, nil
	})
}

// HandleSSHConn handles a Tailscale SSH connection from c.
func (srv *server) HandleSSHConn(c net.Conn) error {
	ss, err := srv.newSSHServer()
	if err != nil {
		return err
	}
	ss.HandleConn(c)

	// Return nil to signal to netstack's interception that it doesn't need to
	// log. If ss.HandleConn had problems, it can log itself (ideally on an
	// sshSession.logf).
	return nil
}

// OnPolicyChange terminates any active sessions that no longer match
// the SSH access policy.
func (srv *server) OnPolicyChange() {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, s := range srv.activeSessionByH {
		go s.checkStillValid()
	}
}

func (srv *server) newSSHServer() (*ssh.Server, error) {
	ss := &ssh.Server{
		Handler:           srv.handleSSH,
		RequestHandlers:   map[string]ssh.RequestHandler{},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{},
		// Note: the direct-tcpip channel handler and LocalPortForwardingCallback
		// only adds support for forwarding ports from the local machine.
		// TODO(maisem/bradfitz): add remote port forwarding support.
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"direct-tcpip": ssh.DirectTCPIPHandler,
		},
		Version:                     "SSH-2.0-Tailscale",
		LocalPortForwardingCallback: srv.mayForwardLocalPortTo,
		NoClientAuthCallback: func(m gossh.ConnMetadata) (*gossh.Permissions, error) {
			if srv.requiresPubKey(m.User(), toIPPort(m.LocalAddr()), toIPPort(m.RemoteAddr())) {
				return nil, errors.New("public key required") // any non-nil error will do
			}
			return nil, nil
		},
		PublicKeyHandler: func(ctx ssh.Context, key ssh.PublicKey) bool {
			if srv.acceptPubKey(ctx.User(), toIPPort(ctx.LocalAddr()), toIPPort(ctx.RemoteAddr()), key) {
				srv.logf("accepting SSH public key %s", bytes.TrimSpace(gossh.MarshalAuthorizedKey(key)))
				return true
			}
			srv.logf("rejecting SSH public key %s", bytes.TrimSpace(gossh.MarshalAuthorizedKey(key)))
			return false
		},
	}
	for k, v := range ssh.DefaultRequestHandlers {
		ss.RequestHandlers[k] = v
	}
	for k, v := range ssh.DefaultChannelHandlers {
		ss.ChannelHandlers[k] = v
	}
	for k, v := range ssh.DefaultSubsystemHandlers {
		ss.SubsystemHandlers[k] = v
	}
	keys, err := srv.lb.GetSSH_HostKeys()
	if err != nil {
		return nil, err
	}
	for _, signer := range keys {
		ss.AddHostKey(signer)
	}
	return ss, nil
}

// mayForwardLocalPortTo reports whether the ctx should be allowed to port forward
// to the specified host and port.
// TODO(bradfitz/maisem): should we have more checks on host/port?
func (srv *server) mayForwardLocalPortTo(ctx ssh.Context, destinationHost string, destinationPort uint32) bool {
	ss, ok := srv.getSessionForContext(ctx)
	if !ok {
		return false
	}
	return ss.action.AllowLocalPortForwarding
}

// requiresPubKey reports whether the SSH server, during the auth negotiation
// phase, should requires that the client send an SSH public key. (or, more
// specifically, that "none" auth isn't acceptable)
func (srv *server) requiresPubKey(sshUser string, localAddr, remoteAddr netaddr.IPPort) bool {
	pol, ok := srv.sshPolicy()
	if !ok {
		return false
	}
	a, ci, _, err := srv.evaluatePolicy(sshUser, localAddr, remoteAddr, nil)
	if err == nil && (a.Accept || a.HoldAndDelegate != "") {
		// Policy doesn't require a public key.
		return false
	}
	if ci == nil {
		// If we didn't get far enough along through evaluatePolicy to know the Tailscale
		// identify of the remote side then it's going to fail quickly later anyway.
		// Return false to accept "none" auth and reject the conn.
		return false
	}

	// Is there any rule that looks like it'd require a public key for this
	// sshUser?
	for _, r := range pol.Rules {
		if ci.ruleExpired(r) {
			continue
		}
		if mapLocalUser(r.SSHUsers, sshUser) == "" {
			continue
		}
		for _, p := range r.Principals {
			if principalMatchesTailscaleIdentity(p, ci) && len(p.PubKeys) > 0 {
				return true
			}
		}
	}
	return false
}

func (srv *server) acceptPubKey(sshUser string, localAddr, remoteAddr netaddr.IPPort, pubKey ssh.PublicKey) bool {
	a, _, _, err := srv.evaluatePolicy(sshUser, localAddr, remoteAddr, pubKey)
	if err != nil {
		return false
	}
	return a.Accept || a.HoldAndDelegate != ""
}

// sshPolicy returns the SSHPolicy for current node.
// If there is no SSHPolicy in the netmap, it returns a debugPolicy
// if one is defined.
func (srv *server) sshPolicy() (_ *tailcfg.SSHPolicy, ok bool) {
	lb := srv.lb
	nm := lb.NetMap()
	if nm == nil {
		return nil, false
	}
	if pol := nm.SSHPolicy; pol != nil && !debugIgnoreTailnetSSHPolicy {
		return pol, true
	}
	if debugPolicyFile != "" {
		f, err := os.ReadFile(debugPolicyFile)
		if err != nil {
			srv.logf("error reading debug SSH policy file: %v", err)
			return nil, false
		}
		p := new(tailcfg.SSHPolicy)
		if err := json.Unmarshal(f, p); err != nil {
			srv.logf("invalid JSON in %v: %v", debugPolicyFile, err)
			return nil, false
		}
		return p, true
	}
	return nil, false
}

func toIPPort(a net.Addr) (ipp netaddr.IPPort) {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return
	}
	tanetaddr, ok := netaddr.FromStdIP(ta.IP)
	if !ok {
		return
	}
	return netaddr.IPPortFrom(tanetaddr, uint16(ta.Port))
}

// evaluatePolicy returns the SSHAction, sshConnInfo and localUser after
// evaluating the sshUser and remoteAddr against the SSHPolicy. The remoteAddr
// and localAddr params must be Tailscale IPs.
//
// The return sshConnInfo will be non-nil, even on some errors, if the
// evaluation made it far enough to resolve the remoteAddr to a Tailscale IP.
func (srv *server) evaluatePolicy(sshUser string, localAddr, remoteAddr netaddr.IPPort, pubKey ssh.PublicKey) (_ *tailcfg.SSHAction, _ *sshConnInfo, localUser string, _ error) {
	pol, ok := srv.sshPolicy()
	if !ok {
		return nil, nil, "", fmt.Errorf("tailssh: rejecting connection; no SSH policy")
	}
	if !tsaddr.IsTailscaleIP(remoteAddr.IP()) {
		return nil, nil, "", fmt.Errorf("tailssh: rejecting non-Tailscale remote address %v", remoteAddr)
	}
	if !tsaddr.IsTailscaleIP(localAddr.IP()) {
		return nil, nil, "", fmt.Errorf("tailssh: rejecting non-Tailscale remote address %v", localAddr)
	}
	node, uprof, ok := srv.lb.WhoIs(remoteAddr)
	if !ok {
		return nil, nil, "", fmt.Errorf("unknown Tailscale identity from src %v", remoteAddr)
	}
	ci := &sshConnInfo{
		now:                srv.now(),
		fetchPublicKeysURL: srv.fetchPublicKeysURL,
		sshUser:            sshUser,
		src:                remoteAddr,
		dst:                localAddr,
		node:               node,
		uprof:              &uprof,
		pubKey:             pubKey,
	}
	a, localUser, ok := evalSSHPolicy(pol, ci)
	if !ok {
		return nil, ci, "", fmt.Errorf("ssh: access denied for %q from %v", uprof.LoginName, ci.src.IP())
	}
	return a, ci, localUser, nil
}

// pubKeyCacheEntry is the cache value for an HTTPS URL of public keys (like
// "https://github.com/foo.keys")
type pubKeyCacheEntry struct {
	lines []string
	etag  string // if sent by server
	at    time.Time
}

const (
	pubKeyCacheDuration      = time.Minute      // how long to cache non-empty public keys
	pubKeyCacheEmptyDuration = 15 * time.Second // how long to cache empty responses
)

func (srv *server) fetchPublicKeysURLCached(url string) (ce pubKeyCacheEntry, ok bool) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	// Mostly don't care about the size of this cache. Clean rarely.
	if m := srv.fetchPublicKeysCache; len(m) > 50 {
		tooOld := srv.now().Add(pubKeyCacheDuration * 10)
		for k, ce := range m {
			if ce.at.Before(tooOld) {
				delete(m, k)
			}
		}
	}
	ce, ok = srv.fetchPublicKeysCache[url]
	if !ok {
		return ce, false
	}
	maxAge := pubKeyCacheDuration
	if len(ce.lines) == 0 {
		maxAge = pubKeyCacheEmptyDuration
	}
	return ce, srv.now().Sub(ce.at) < maxAge
}

func (srv *server) pubKeyClient() *http.Client {
	if srv.pubKeyHTTPClient != nil {
		return srv.pubKeyHTTPClient
	}
	return http.DefaultClient
}

func (srv *server) fetchPublicKeysURL(url string) ([]string, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, errors.New("invalid URL scheme")
	}

	ce, ok := srv.fetchPublicKeysURLCached(url)
	if ok {
		return ce.lines, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	if ce.etag != "" {
		req.Header.Add("If-None-Match", ce.etag)
	}
	res, err := srv.pubKeyClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var lines []string
	var etag string
	switch res.StatusCode {
	default:
		err = fmt.Errorf("unexpected status %v", res.Status)
		srv.logf("fetching public keys from %s: %v", url, err)
	case http.StatusNotModified:
		lines = ce.lines
		etag = ce.etag
	case http.StatusOK:
		var all []byte
		all, err = io.ReadAll(io.LimitReader(res.Body, 4<<10))
		if s := strings.TrimSpace(string(all)); s != "" {
			lines = strings.Split(s, "\n")
		}
		etag = res.Header.Get("Etag")
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	mapSet(&srv.fetchPublicKeysCache, url, pubKeyCacheEntry{
		at:    srv.now(),
		lines: lines,
		etag:  etag,
	})
	return lines, err
}

// handleSSH is invoked when a new SSH connection attempt is made.
func (srv *server) handleSSH(s ssh.Session) {
	logf := srv.logf

	sshUser := s.User()
	action, ci, localUser, err := srv.evaluatePolicy(sshUser, toIPPort(s.LocalAddr()), toIPPort(s.RemoteAddr()), s.PublicKey())
	if err != nil {
		logf(err.Error())
		s.Exit(1)
		return
	}
	var lu *user.User
	if localUser != "" {
		lu, err = user.Lookup(localUser)
		if err != nil {
			logf("ssh: user Lookup %q: %v", localUser, err)
			s.Exit(1)
			return
		}
	}
	ss := srv.newSSHSession(s, ci, lu)
	ss.logf("handling new SSH connection from %v (%v) to ssh-user %q", ci.uprof.LoginName, ci.src.IP(), sshUser)
	action, err = ss.resolveTerminalAction(action)
	if err != nil {
		ss.logf("resolveTerminalAction: %v", err)
		io.WriteString(s.Stderr(), "Access denied: failed to resolve SSHAction.\n")
		s.Exit(1)
		return
	}
	if action.Reject || !action.Accept {
		ss.logf("access denied for %v (%v)", ci.uprof.LoginName, ci.src.IP())
		s.Exit(1)
		return
	}
	ss.logf("access granted for %v (%v) to ssh-user %q", ci.uprof.LoginName, ci.src.IP(), sshUser)
	ss.action = action
	ss.run()
}

// resolveTerminalAction either returns action (if it's Accept or Reject) or else
// loops, fetching new SSHActions from the control plane.
//
// Any action with a Message in the chain will be printed to ss.
//
// The returned SSHAction will be either Reject or Accept.
func (ss *sshSession) resolveTerminalAction(action *tailcfg.SSHAction) (*tailcfg.SSHAction, error) {
	// Loop processing/fetching Actions until one reaches a
	// terminal state (Accept, Reject, or invalid Action), or
	// until fetchSSHAction times out due to the context being
	// done (client disconnect) or its 30 minute timeout passes.
	// (Which is a long time for somebody to see login
	// instructions and go to a URL to do something.)
	for {
		if action.Message != "" {
			io.WriteString(ss.Stderr(), strings.Replace(action.Message, "\n", "\r\n", -1))
		}
		if action.Accept || action.Reject {
			return action, nil
		}
		url := action.HoldAndDelegate
		if url == "" {
			return nil, errors.New("reached Action that lacked Accept, Reject, and HoldAndDelegate")
		}
		url = ss.expandDelegateURL(url)
		var err error
		action, err = ss.srv.fetchSSHAction(ss.Context(), url)
		if err != nil {
			return nil, fmt.Errorf("fetching SSHAction from %s: %w", url, err)
		}
	}
}

func (ss *sshSession) expandDelegateURL(actionURL string) string {
	nm := ss.srv.lb.NetMap()
	var dstNodeID string
	if nm != nil {
		dstNodeID = fmt.Sprint(int64(nm.SelfNode.ID))
	}
	return strings.NewReplacer(
		"$SRC_NODE_IP", url.QueryEscape(ss.connInfo.src.IP().String()),
		"$SRC_NODE_ID", fmt.Sprint(int64(ss.connInfo.node.ID)),
		"$DST_NODE_IP", url.QueryEscape(ss.connInfo.dst.IP().String()),
		"$DST_NODE_ID", dstNodeID,
		"$SSH_USER", url.QueryEscape(ss.connInfo.sshUser),
		"$LOCAL_USER", url.QueryEscape(ss.localUser.Username),
	).Replace(actionURL)
}

// sshSession is an accepted Tailscale SSH session.
type sshSession struct {
	ssh.Session
	idH      string // the RFC4253 sec8 hash H; don't share outside process
	sharedID string // ID that's shared with control
	logf     logger.Logf

	ctx           *sshContext // implements context.Context
	srv           *server
	connInfo      *sshConnInfo
	action        *tailcfg.SSHAction
	localUser     *user.User
	agentListener net.Listener // non-nil if agent-forwarding requested+allowed

	// initialized by launchProcess:
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader // nil for pty sessions
	ptyReq *ssh.Pty  // non-nil for pty sessions

	// We use this sync.Once to ensure that we only terminate the process once,
	// either it exits itself or is terminated
	exitOnce sync.Once
}

func (ss *sshSession) vlogf(format string, args ...interface{}) {
	if sshVerboseLogging {
		ss.logf(format, args...)
	}
}

func (srv *server) newSSHSession(s ssh.Session, ci *sshConnInfo, lu *user.User) *sshSession {
	sharedID := fmt.Sprintf("%s-%02x", ci.now.UTC().Format("20060102T150405"), randBytes(5))
	return &sshSession{
		Session:   s,
		idH:       s.Context().(ssh.Context).SessionID(),
		sharedID:  sharedID,
		ctx:       newSSHContext(),
		srv:       srv,
		localUser: lu,
		connInfo:  ci,
		logf:      logger.WithPrefix(srv.logf, "ssh-session("+sharedID+"): "),
	}
}

// checkStillValid checks that the session is still valid per the latest SSHPolicy.
// If not, it terminates the session.
func (ss *sshSession) checkStillValid() {
	ci := ss.connInfo
	a, _, lu, err := ss.srv.evaluatePolicy(ci.sshUser, ci.src, ci.dst, ci.pubKey)
	if err == nil && (a.Accept || a.HoldAndDelegate != "") && lu == ss.localUser.Username {
		return
	}
	ss.logf("session no longer valid per new SSH policy; closing")
	ss.ctx.CloseWithError(userVisibleError{
		fmt.Sprintf("Access revoked.\n"),
		context.Canceled,
	})
}

func (srv *server) fetchSSHAction(ctx context.Context, url string) (*tailcfg.SSHAction, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	bo := backoff.NewBackoff("fetch-ssh-action", srv.logf, 10*time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		res, err := srv.lb.DoNoiseRequest(req)
		if err != nil {
			bo.BackOff(ctx, err)
			continue
		}
		if res.StatusCode != 200 {
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if len(body) > 1<<10 {
				body = body[:1<<10]
			}
			srv.logf("fetch of %v: %s, %s", url, res.Status, body)
			bo.BackOff(ctx, fmt.Errorf("unexpected status: %v", res.Status))
			continue
		}
		a := new(tailcfg.SSHAction)
		err = json.NewDecoder(res.Body).Decode(a)
		res.Body.Close()
		if err != nil {
			srv.logf("invalid next SSHAction JSON from %v: %v", url, err)
			bo.BackOff(ctx, err)
			continue
		}
		return a, nil
	}
}

// killProcessOnContextDone waits for ss.ctx to be done and kills the process,
// unless the process has already exited.
func (ss *sshSession) killProcessOnContextDone() {
	<-ss.ctx.Done()
	// Either the process has already existed, in which case this does nothing.
	// Or, the process is still running in which case this will kill it.
	ss.exitOnce.Do(func() {
		err := ss.ctx.Err()
		if serr, ok := err.(SSHTerminationError); ok {
			msg := serr.SSHTerminationMessage()
			if msg != "" {
				io.WriteString(ss.Stderr(), "\r\n\r\n"+msg+"\r\n\r\n")
			}
		}
		ss.logf("terminating SSH session from %v: %v", ss.connInfo.src.IP(), err)
		ss.cmd.Process.Kill()
	})
}

// sessionAction returns the SSHAction associated with the session.
func (srv *server) getSessionForContext(sctx ssh.Context) (ss *sshSession, ok bool) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	ss, ok = srv.activeSessionByH[sctx.SessionID()]
	return
}

// startSession registers ss as an active session.
func (srv *server) startSession(ss *sshSession) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if ss.idH == "" {
		panic("empty idH")
	}
	if ss.sharedID == "" {
		panic("empty sharedID")
	}
	if _, dup := srv.activeSessionByH[ss.idH]; dup {
		panic("dup idH")
	}
	if _, dup := srv.activeSessionBySharedID[ss.sharedID]; dup {
		panic("dup sharedID")
	}
	mapSet(&srv.activeSessionByH, ss.idH, ss)
	mapSet(&srv.activeSessionBySharedID, ss.sharedID, ss)
}

// endSession unregisters s from the list of active sessions.
func (srv *server) endSession(ss *sshSession) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	delete(srv.activeSessionByH, ss.idH)
	delete(srv.activeSessionBySharedID, ss.sharedID)
}

var errSessionDone = errors.New("session is done")

// handleSSHAgentForwarding starts a Unix socket listener and in the background
// forwards agent connections between the listenr and the ssh.Session.
// On success, it assigns ss.agentListener.
func (ss *sshSession) handleSSHAgentForwarding(s ssh.Session, lu *user.User) error {
	if !ssh.AgentRequested(ss) || !ss.action.AllowAgentForwarding {
		return nil
	}
	ss.logf("ssh: agent forwarding requested")
	ln, err := ssh.NewAgentListener()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil && ln != nil {
			ln.Close()
		}
	}()

	uid, err := strconv.ParseUint(lu.Uid, 10, 32)
	if err != nil {
		return err
	}
	gid, err := strconv.ParseUint(lu.Gid, 10, 32)
	if err != nil {
		return err
	}
	socket := ln.Addr().String()
	dir := filepath.Dir(socket)
	// Make sure the socket is accessible by the user.
	if err := os.Chown(socket, int(uid), int(gid)); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0755); err != nil {
		return err
	}

	go ssh.ForwardAgentConnections(ln, s)
	ss.agentListener = ln
	return nil
}

// recordSSH is a temporary dev knob to test the SSH recording
// functionality and support off-node streaming.
//
// TODO(bradfitz,maisem): move this to SSHPolicy.
var recordSSH = envknob.Bool("TS_DEBUG_LOG_SSH")

// run is the entrypoint for a newly accepted SSH session.
//
// It handles ss once it's been accepted and determined
// that it should run.
func (ss *sshSession) run() {
	srv := ss.srv
	srv.startSession(ss)
	defer srv.endSession(ss)

	defer ss.ctx.CloseWithError(errSessionDone)

	if ss.action.SesssionDuration != 0 {
		t := time.AfterFunc(ss.action.SesssionDuration, func() {
			ss.ctx.CloseWithError(userVisibleError{
				fmt.Sprintf("Session timeout of %v elapsed.", ss.action.SesssionDuration),
				context.DeadlineExceeded,
			})
		})
		defer t.Stop()
	}

	logf := srv.logf
	lu := ss.localUser
	localUser := lu.Username

	if euid := os.Geteuid(); euid != 0 {
		if lu.Uid != fmt.Sprint(euid) {
			ss.logf("can't switch to user %q from process euid %v", localUser, euid)
			fmt.Fprintf(ss, "can't switch user\n")
			ss.Exit(1)
			return
		}
	}

	// Take control of the PTY so that we can configure it below.
	// See https://github.com/tailscale/tailscale/issues/4146
	ss.DisablePTYEmulation()

	if err := ss.handleSSHAgentForwarding(ss, lu); err != nil {
		ss.logf("agent forwarding failed: %v", err)
	} else if ss.agentListener != nil {
		// TODO(maisem/bradfitz): add a way to close all session resources
		defer ss.agentListener.Close()
	}

	var rec *recording // or nil if disabled
	if ss.shouldRecord() {
		var err error
		rec, err = ss.startNewRecording()
		if err != nil {
			fmt.Fprintf(ss, "can't start new recording\n")
			ss.logf("startNewRecording: %v", err)
			ss.Exit(1)
			return
		}
		defer rec.Close()
	}

	err := ss.launchProcess(ss.ctx)
	if err != nil {
		logf("start failed: %v", err.Error())
		ss.Exit(1)
		return
	}
	go ss.killProcessOnContextDone()

	go func() {
		_, err := io.Copy(rec.writer("i", ss.stdin), ss)
		if err != nil {
			// TODO: don't log in the success case.
			logf("ssh: stdin copy: %v", err)
		}
		ss.stdin.Close()
	}()
	go func() {
		_, err := io.Copy(rec.writer("o", ss), ss.stdout)
		if err != nil {
			// TODO: don't log in the success case.
			logf("ssh: stdout copy: %v", err)
		}
	}()
	// stderr is nil for ptys.
	if ss.stderr != nil {
		go func() {
			_, err := io.Copy(ss.Stderr(), ss.stderr)
			if err != nil {
				// TODO: don't log in the success case.
				logf("ssh: stderr copy: %v", err)
			}
		}()
	}
	err = ss.cmd.Wait()
	// This will either make the SSH Termination goroutine be a no-op,
	// or itself will be a no-op because the process was killed by the
	// aforementioned goroutine.
	ss.exitOnce.Do(func() {})

	if err == nil {
		ss.logf("Wait: ok")
		ss.Exit(0)
		return
	}
	if ee, ok := err.(*exec.ExitError); ok {
		code := ee.ProcessState.ExitCode()
		ss.logf("Wait: code=%v", code)
		ss.Exit(code)
		return
	}

	ss.logf("Wait: %v", err)
	ss.Exit(1)
	return
}

func (ss *sshSession) shouldRecord() bool {
	// for now only record pty sessions
	// TODO(bradfitz,maisem): make configurable on SSHPolicy and
	// support recording non-pty stuff too.
	_, _, isPtyReq := ss.Pty()
	return recordSSH && isPtyReq
}

type sshConnInfo struct {
	// now is the time to consider the present moment for the
	// purposes of rule evaluation.
	now time.Time
	// fetchPublicKeysURL, if non-nil, is a func to fetch the public
	// keys of a URL. The strings are in the the typical public
	// key "type base64-string [comment]" format seen at e.g. https://github.com/USER.keys
	fetchPublicKeysURL func(url string) ([]string, error)

	// sshUser is the requested local SSH username ("root", "alice", etc).
	sshUser string

	// src is the Tailscale IP and port that the connection came from.
	src netaddr.IPPort

	// dst is the Tailscale IP and port that the connection came for.
	dst netaddr.IPPort

	// node is srcIP's node.
	node *tailcfg.Node

	// uprof is node's UserProfile.
	uprof *tailcfg.UserProfile

	// pubKey is the public key presented by the client, or nil
	// if they haven't yet sent one (as in the early "none" phase
	// of authentication negotiation).
	pubKey ssh.PublicKey
}

func (ci *sshConnInfo) ruleExpired(r *tailcfg.SSHRule) bool {
	if r.RuleExpires == nil {
		return false
	}
	return r.RuleExpires.Before(ci.now)
}

func evalSSHPolicy(pol *tailcfg.SSHPolicy, ci *sshConnInfo) (a *tailcfg.SSHAction, localUser string, ok bool) {
	for _, r := range pol.Rules {
		if a, localUser, err := matchRule(r, ci); err == nil {
			return a, localUser, true
		}
	}
	return nil, "", false
}

// internal errors for testing; they don't escape to callers or logs.
var (
	errNilRule        = errors.New("nil rule")
	errNilAction      = errors.New("nil action")
	errRuleExpired    = errors.New("rule expired")
	errPrincipalMatch = errors.New("principal didn't match")
	errUserMatch      = errors.New("user didn't match")
)

func matchRule(r *tailcfg.SSHRule, ci *sshConnInfo) (a *tailcfg.SSHAction, localUser string, err error) {
	if r == nil {
		return nil, "", errNilRule
	}
	if r.Action == nil {
		return nil, "", errNilAction
	}
	if ci.ruleExpired(r) {
		return nil, "", errRuleExpired
	}
	if !r.Action.Reject || r.SSHUsers != nil {
		localUser = mapLocalUser(r.SSHUsers, ci.sshUser)
		if localUser == "" {
			return nil, "", errUserMatch
		}
	}
	if !anyPrincipalMatches(r.Principals, ci) {
		return nil, "", errPrincipalMatch
	}
	return r.Action, localUser, nil
}

func mapLocalUser(ruleSSHUsers map[string]string, reqSSHUser string) (localUser string) {
	v, ok := ruleSSHUsers[reqSSHUser]
	if !ok {
		v = ruleSSHUsers["*"]
	}
	if v == "=" {
		return reqSSHUser
	}
	return v
}

func anyPrincipalMatches(ps []*tailcfg.SSHPrincipal, ci *sshConnInfo) bool {
	for _, p := range ps {
		if p == nil {
			continue
		}
		if principalMatches(p, ci) {
			return true
		}
	}
	return false
}

func principalMatches(p *tailcfg.SSHPrincipal, ci *sshConnInfo) bool {
	return principalMatchesTailscaleIdentity(p, ci) &&
		principalMatchesPubKey(p, ci)
}

// principalMatchesTailscaleIdentity reports whether one of p's four fields
// that match the Tailscale identity match (Node, NodeIP, UserLogin, Any).
// This function does not consider PubKeys.
func principalMatchesTailscaleIdentity(p *tailcfg.SSHPrincipal, ci *sshConnInfo) bool {
	if p.Any {
		return true
	}
	if !p.Node.IsZero() && ci.node != nil && p.Node == ci.node.StableID {
		return true
	}
	if p.NodeIP != "" {
		if ip, _ := netaddr.ParseIP(p.NodeIP); ip == ci.src.IP() {
			return true
		}
	}
	if p.UserLogin != "" && ci.uprof != nil && ci.uprof.LoginName == p.UserLogin {
		return true
	}
	return false
}

func principalMatchesPubKey(p *tailcfg.SSHPrincipal, ci *sshConnInfo) bool {
	if len(p.PubKeys) == 0 {
		return true
	}
	if ci.pubKey == nil {
		return false
	}
	pubKeys := p.PubKeys
	if len(pubKeys) == 1 && strings.HasPrefix(pubKeys[0], "https://") {
		if ci.fetchPublicKeysURL == nil {
			// TODO: log?
			return false
		}
		var err error
		pubKeys, err = ci.fetchPublicKeysURL(pubKeys[0])
		if err != nil {
			// TODO: log?
			return false
		}
	}
	for _, pubKey := range pubKeys {
		if pubKeyMatchesAuthorizedKey(ci.pubKey, pubKey) {
			return true
		}
	}
	return false
}

func pubKeyMatchesAuthorizedKey(pubKey ssh.PublicKey, wantKey string) bool {
	wantKeyType, rest, ok := strings.Cut(wantKey, " ")
	if !ok {
		return false
	}
	if pubKey.Type() != wantKeyType {
		return false
	}
	wantKeyB64, _, _ := strings.Cut(rest, " ")
	wantKeyData, _ := base64.StdEncoding.DecodeString(wantKeyB64)
	return len(wantKeyData) > 0 && bytes.Equal(pubKey.Marshal(), wantKeyData)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// startNewRecording starts a new SSH session recording.
//
// It writes an asciinema file to
// $TAILSCALE_VAR_ROOT/ssh-sessions/ssh-session-<unixtime>-*.cast.
func (ss *sshSession) startNewRecording() (*recording, error) {
	var w ssh.Window
	if ptyReq, _, isPtyReq := ss.Pty(); isPtyReq {
		w = ptyReq.Window
	}

	term := envValFromList(ss.Environ(), "TERM")
	if term == "" {
		term = "xterm-256color" // something non-empty
	}

	now := time.Now()
	rec := &recording{
		ss:    ss,
		start: now,
	}
	varRoot := ss.srv.lb.TailscaleVarRoot()
	if varRoot == "" {
		return nil, errors.New("no var root for recording storage")
	}
	dir := filepath.Join(varRoot, "ssh-sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := ioutil.TempFile(dir, fmt.Sprintf("ssh-session-%v-*.cast", now.UnixNano()))
	if err != nil {
		return nil, err
	}
	rec.out = f

	// {"version": 2, "width": 221, "height": 84, "timestamp": 1647146075, "env": {"SHELL": "/bin/bash", "TERM": "screen"}}
	type CastHeader struct {
		Version   int               `json:"version"`
		Width     int               `json:"width"`
		Height    int               `json:"height"`
		Timestamp int64             `json:"timestamp"`
		Env       map[string]string `json:"env"`
	}
	j, err := json.Marshal(CastHeader{
		Version:   2,
		Width:     w.Width,
		Height:    w.Height,
		Timestamp: now.Unix(),
		Env: map[string]string{
			"TERM": term,
			// TODO(bradiftz): anything else important?
			// including all seems noisey, but maybe we should
			// for auditing. But first need to break
			// launchProcess's startWithStdPipes and
			// startWithPTY up so that they first return the cmd
			// without starting it, and then a step that starts
			// it. Then we can (1) make the cmd, (2) start the
			// recording, (3) start the process.
		},
	})
	if err != nil {
		f.Close()
		return nil, err
	}
	ss.logf("starting asciinema recording to %s", f.Name())
	j = append(j, '\n')
	if _, err := f.Write(j); err != nil {
		f.Close()
		return nil, err
	}
	return rec, nil
}

// recording is the state for an SSH session recording.
type recording struct {
	ss    *sshSession
	start time.Time

	mu  sync.Mutex // guards writes to, close of out
	out *os.File   // nil if closed
}

func (r *recording) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.out == nil {
		return nil
	}
	err := r.out.Close()
	r.out = nil
	return err
}

// writer returns an io.Writer around w that first records the write.
//
// The dir should be "i" for input or "o" for output.
//
// If r is nil, it returns w unchanged.
func (r *recording) writer(dir string, w io.Writer) io.Writer {
	if r == nil {
		return w
	}
	return &loggingWriter{r, dir, w}
}

// loggingWriter is an io.Writer wrapper that writes first an
// asciinema JSON cast format recording line, and then writes to w.
type loggingWriter struct {
	r   *recording
	dir string    // "i" or "o" (input or output)
	w   io.Writer // underlying Writer, after writing to r.out
}

func (w loggingWriter) Write(p []byte) (n int, err error) {
	j, err := json.Marshal([]interface{}{
		time.Since(w.r.start).Seconds(),
		w.dir,
		string(p),
	})
	if err != nil {
		return 0, err
	}
	j = append(j, '\n')
	if err := w.writeCastLine(j); err != nil {
		return 0, nil
	}
	return w.w.Write(p)
}

func (w loggingWriter) writeCastLine(j []byte) error {
	w.r.mu.Lock()
	defer w.r.mu.Unlock()
	if w.r.out == nil {
		return errors.New("logger closed")
	}
	_, err := w.r.out.Write(j)
	if err != nil {
		return fmt.Errorf("logger Write: %w", err)
	}
	return nil
}

func envValFromList(env []string, wantKey string) (v string) {
	for _, kv := range env {
		if thisKey, v, ok := strings.Cut(kv, "="); ok && envEq(thisKey, wantKey) {
			return v
		}
	}
	return ""
}

// envEq reports whether environment variable a == b for the current
// operating system.
func envEq(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// mapSet assigns m[k] = v, making m if necessary.
func mapSet[K comparable, V any](m *map[K]V, k K, v V) {
	if *m == nil {
		*m = make(map[K]V)
	}
	(*m)[k] = v
}
