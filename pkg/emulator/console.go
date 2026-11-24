package emulator

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The emulator console (telnet protocol on the console port) reaches the
// virtual hardware from outside Android: the fingerprint sensor, and the
// display itself. Android blanks secure windows (BiometricPrompt, the PIN
// entry, FLAG_SECURE apps) in `adb screencap`; the console screenshot reads
// the emulator's framebuffer, so it shows them.

// console is one authenticated console connection.
type console struct {
	conn net.Conn
	r    *bufio.Reader
}

// dialConsole connects to the emulator console on 127.0.0.1:port and
// authenticates with the token file the console names in its banner.
func dialConsole(ctx context.Context, port int) (*console, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("console: %w", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	conn.SetDeadline(deadline)
	c := &console{conn: conn, r: bufio.NewReader(conn)}
	banner, err := c.reply()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("console banner: %w", err)
	}
	if strings.Contains(banner, "Authentication required") {
		path := tokenPath(banner)
		if path == "" {
			conn.Close()
			return nil, fmt.Errorf("console asks for a token but names no token file: %q", banner)
		}
		token, err := os.ReadFile(path)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("console token: %w", err)
		}
		if _, err := c.do("auth " + strings.TrimSpace(string(token))); err != nil {
			conn.Close()
			return nil, fmt.Errorf("console auth: %w", err)
		}
	}
	return c, nil
}

// tokenPath finds the quoted token file path in the console banner:
//
//	Android Console: you can find your <auth_token> in
//	'/home/u/.emulator_console_auth_token'
func tokenPath(banner string) string {
	for _, line := range strings.Split(banner, "\n") {
		line = strings.TrimSpace(line)
		if len(line) > 2 && line[0] == '\'' && line[len(line)-1] == '\'' {
			return filepath.Clean(line[1 : len(line)-1])
		}
	}
	return ""
}

// reply reads lines up to the terminating "OK" or "KO: ..." line.
func (c *console) reply() (string, error) {
	var b strings.Builder
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return b.String(), err
		}
		t := strings.TrimRight(line, "\r\n")
		switch {
		case t == "OK":
			return b.String(), nil
		case strings.HasPrefix(t, "KO"):
			return b.String(), errors.New(strings.TrimSpace(strings.TrimPrefix(t, "KO:")))
		}
		b.WriteString(t)
		b.WriteByte('\n')
	}
}

// do sends one command and returns its output.
func (c *console) do(cmd string) (string, error) {
	if _, err := c.conn.Write([]byte(cmd + "\r\n")); err != nil {
		return "", err
	}
	return c.reply()
}

func (c *console) close() {
	c.conn.Write([]byte("quit\r\n"))
	c.conn.Close()
}

// Console runs emulator console commands in order and returns their output.
func (e *Emulator) Console(ctx context.Context, cmds ...string) (string, error) {
	c, err := dialConsole(ctx, e.Dev.ConsolePort)
	if err != nil {
		return "", err
	}
	defer c.close()
	var out strings.Builder
	for _, cmd := range cmds {
		s, err := c.do(cmd)
		out.WriteString(s)
		if err != nil {
			return out.String(), fmt.Errorf("console %q: %w", cmd, err)
		}
	}
	return out.String(), nil
}

// FingerTouch puts finger id on the virtual fingerprint sensor and lifts it,
// as a real touch does. It satisfies a fingerprint prompt (BiometricPrompt)
// when that finger is enrolled, and advances fingerprint enrollment.
func (e *Emulator) FingerTouch(ctx context.Context, id int) error {
	c, err := dialConsole(ctx, e.Dev.ConsolePort)
	if err != nil {
		return err
	}
	defer c.close()
	if _, err := c.do(fmt.Sprintf("finger touch %d", id)); err != nil {
		return fmt.Errorf("finger touch: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(500 * time.Millisecond):
	}
	if _, err := c.do(fmt.Sprintf("finger remove %d", id)); err != nil {
		return fmt.Errorf("finger remove: %w", err)
	}
	return nil
}

// HostScreenshot captures the emulator display from outside Android, so
// secure windows that `adb screencap` returns as black are visible.
func (e *Emulator) HostScreenshot(ctx context.Context) ([]byte, error) {
	if err := os.MkdirAll(e.Cfg.DeviceDir(e.Name), 0o700); err != nil {
		return nil, err
	}
	// The emulator process writes the file, so the directory must be one it
	// can write: the device's own state directory.
	dir, err := os.MkdirTemp(e.Cfg.DeviceDir(e.Name), "shot-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if _, err := e.Console(ctx, "screenrecord screenshot "+dir); err != nil {
		return nil, err
	}
	// The emulator answers OK before the file is complete: wait for a
	// complete PNG (it ends with the IEND chunk).
	for {
		m, _ := filepath.Glob(filepath.Join(dir, "*.png"))
		if len(m) > 0 {
			if b, err := os.ReadFile(m[0]); err == nil && isCompletePNG(b) {
				return b, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("host screenshot: no image written to %s: %w", dir, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func isCompletePNG(b []byte) bool {
	return len(b) > 20 && strings.HasPrefix(string(b), "\x89PNG") &&
		strings.HasSuffix(string(b), "IEND\xaeB`\x82")
}
