package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// sshTarget is parsed from EMUCTL_URL=ssh://[user@]host[:port][/api-host:api-port].
// The client reaches the API, and leased devices' adb ports, through SSH
// local forwards, so the server only needs to listen on loopback.
type sshTarget struct {
	dest   string // [user@]host for ssh
	port   string // ssh port, "" for default
	remote string // API address as seen from the server
}

func parseSSH(raw string) (*sshTarget, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad EMUCTL_URL %q: want ssh://[user@]host[:port][/api-host:api-port]", raw)
	}
	t := &sshTarget{dest: u.Hostname(), port: u.Port(), remote: "127.0.0.1:7600"}
	if u.User != nil {
		t.dest = u.User.Username() + "@" + t.dest
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		if _, _, err := net.SplitHostPort(p); err != nil {
			return nil, fmt.Errorf("bad API address %q in EMUCTL_URL: want host:port", p)
		}
		t.remote = p
	}
	return t, nil
}

type forward struct {
	local string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{} // closed when ssh exits
}

func (f *forward) alive() bool {
	select {
	case <-f.done:
		return false
	default:
		return true
	}
}

func (f *forward) close() {
	f.stdin.Close()
	f.cmd.Process.Kill()
	<-f.done
}

// open starts `ssh -L 127.0.0.1:PORT:remote dest` and waits until the local
// end accepts connections. The remote side runs `cat`, reading our stdin, so
// the tunnel also goes away if this process dies without cleaning up.
func (t *sshTarget) open(ctx context.Context, remote string) (*forward, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	local := ln.Addr().String()
	ln.Close()
	args := []string{"-o", "ExitOnForwardFailure=yes", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15",
		"-L", local + ":" + remote}
	if t.port != "" {
		args = append(args, "-p", t.port)
	}
	args = append(args, t.dest, "cat >/dev/null")
	cmd := exec.Command("ssh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ssh: %w", err)
	}
	f := &forward{local: local, cmd: cmd, stdin: stdin, done: make(chan struct{})}
	go func() { cmd.Wait(); close(f.done) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if c, err := net.DialTimeout("tcp", local, time.Second); err == nil {
			c.Close()
			return f, nil
		}
		select {
		case <-f.done:
			return nil, fmt.Errorf("ssh tunnel to %s (%s) failed: %s", t.dest, remote, strings.TrimSpace(stderr.String()))
		case <-ctx.Done():
			f.close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			f.close()
			return nil, fmt.Errorf("ssh tunnel to %s (%s): timed out: %s", t.dest, remote, strings.TrimSpace(stderr.String()))
		}
	}
}

type tunnels struct {
	mu  sync.Mutex
	api *forward
	adb map[string]*forward // remote adb address -> forward
}

// ensureAPI opens the API tunnel on first use and points Base at it.
func (c *Client) ensureAPI(ctx context.Context) error {
	if c.ssh == nil {
		return nil
	}
	c.tun.mu.Lock()
	defer c.tun.mu.Unlock()
	if c.tun.api != nil {
		if c.tun.api.alive() {
			return nil
		}
		c.tun.api = nil // ssh exited, reconnect
	}
	f, err := c.ssh.open(ctx, c.ssh.remote)
	if err != nil {
		return err
	}
	c.tun.api = f
	c.Base = "http://" + f.local
	return nil
}

// LocalADB rewrites l.ADB/l.Serial to an address reachable from this
// machine. Over ssh:// that is a local forward to the device's adb port on
// the server; otherwise l is returned unchanged.
func (c *Client) LocalADB(ctx context.Context, l Lease) (Lease, error) {
	if c.ssh == nil || l.ADB == "" {
		return l, nil
	}
	_, port, err := net.SplitHostPort(l.ADB)
	if err != nil {
		return l, err
	}
	local, err := c.ADBAddr(ctx, net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return l, err
	}
	l.ADB, l.Serial = local, local
	return l, nil
}

// ADBAddr returns an address this machine can `adb connect` to for a lease's
// adb target. Over ssh:// it opens (and reuses) a local forward to it.
func (c *Client) ADBAddr(ctx context.Context, remote string) (string, error) {
	if c.ssh == nil || remote == "" {
		return remote, nil
	}
	c.tun.mu.Lock()
	defer c.tun.mu.Unlock()
	if c.tun.adb == nil {
		c.tun.adb = map[string]*forward{}
	}
	if f, ok := c.tun.adb[remote]; ok && f.alive() {
		return f.local, nil
	}
	f, err := c.ssh.open(ctx, remote)
	if err != nil {
		return "", err
	}
	c.tun.adb[remote] = f
	return f.local, nil
}

// Close stops any SSH tunnels the client opened.
func (c *Client) Close() {
	if c.ssh == nil {
		return
	}
	c.tun.mu.Lock()
	defer c.tun.mu.Unlock()
	if c.tun.api != nil {
		c.tun.api.close()
		c.tun.api = nil
	}
	for k, f := range c.tun.adb {
		f.close()
		delete(c.tun.adb, k)
	}
}
