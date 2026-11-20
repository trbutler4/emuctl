// Package client is the Go client for the emuctl API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/trbutler4/emuctl/pkg/config"
)

type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// FromEnv uses EMUCTL_URL (default http://127.0.0.1:7600) and the token from
// EMUCTL_TOKEN, else the file EMUCTL_TOKEN_FILE, else ~/.config/emuctl/token.
func FromEnv() *Client {
	base := os.Getenv("EMUCTL_URL")
	if base == "" {
		base = "http://127.0.0.1:7600"
	}
	token := os.Getenv("EMUCTL_TOKEN")
	if token == "" {
		file := os.Getenv("EMUCTL_TOKEN_FILE")
		if file == "" {
			file = filepath.Join(config.ConfigDir(), "token")
		}
		if b, err := os.ReadFile(file); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	return &Client{Base: strings.TrimRight(base, "/"), Token: token,
		HTTP: &http.Client{Timeout: 10 * time.Minute}}
}

type Lease struct {
	ID       string    `json:"id"`
	User     string    `json:"user"`
	Want     string    `json:"want"`
	Device   string    `json:"device"`
	State    string    `json:"state"`
	ClientIP string    `json:"client_ip"`
	Note     string    `json:"note"`
	Position int       `json:"position"`
	Expires  time.Time `json:"expires_at"`
	ADB      string    `json:"adb"`
	Serial   string    `json:"serial"`
	Ready    bool      `json:"ready"`
}

type Device struct {
	Name     string    `json:"name"`
	State    string    `json:"state"`
	Serial   string    `json:"serial"`
	ADB      string    `json:"adb"`
	Profile  string    `json:"profile"`
	Since    time.Time `json:"since"`
	Restarts int       `json:"restarts"`
	LastErr  string    `json:"last_error"`
	Holder   *Lease    `json:"holder"`
}

func (c *Client) Do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != 200 {
		var e struct{ Error string }
		json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, e.Error)
	}
	if out == nil {
		return nil
	}
	if p, ok := out.(*[]byte); ok {
		*p = b
		return nil
	}
	return json.Unmarshal(b, out)
}

func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var out []Device
	return out, c.Do(ctx, "GET", "/v1/devices", nil, &out)
}

func (c *Client) Queue(ctx context.Context) ([]Lease, error) {
	var out []Lease
	return out, c.Do(ctx, "GET", "/v1/queue", nil, &out)
}

func (c *Client) Request(ctx context.Context, device, note string) (Lease, error) {
	b, _ := json.Marshal(map[string]string{"device": device, "note": note})
	var l Lease
	return l, c.Do(ctx, "POST", "/v1/leases", bytes.NewReader(b), &l)
}

func (c *Client) Get(ctx context.Context, id string, wait time.Duration) (Lease, error) {
	var l Lease
	return l, c.Do(ctx, "GET", "/v1/leases/"+url.PathEscape(id)+"?wait="+wait.String(), nil, &l)
}

func (c *Client) Renew(ctx context.Context, id string) (Lease, error) {
	var l Lease
	return l, c.Do(ctx, "POST", "/v1/leases/"+url.PathEscape(id)+"/renew", nil, &l)
}

func (c *Client) Release(ctx context.Context, id string) error {
	return c.Do(ctx, "DELETE", "/v1/leases/"+url.PathEscape(id), nil, nil)
}

func (c *Client) Install(ctx context.Context, id string, apk io.Reader) (map[string]any, error) {
	var out map[string]any
	return out, c.Do(ctx, "POST", "/v1/leases/"+url.PathEscape(id)+"/install", apk, &out)
}

func (c *Client) Reset(ctx context.Context, id string, wipe bool) (Lease, error) {
	q := ""
	if wipe {
		q = "?wipe=1"
	}
	var l Lease
	return l, c.Do(ctx, "POST", "/v1/leases/"+url.PathEscape(id)+"/reset"+q, nil, &l)
}

// ShellResult is the output of a command run on a leased device.
type ShellResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

// Shell runs `adb shell CMD` on the leased device, on the server.
func (c *Client) Shell(ctx context.Context, id, cmd string) (ShellResult, error) {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	var out ShellResult
	return out, c.Do(ctx, "POST", "/v1/leases/"+url.PathEscape(id)+"/shell", bytes.NewReader(b), &out)
}

// Screenshot returns a PNG of the leased device's screen.
func (c *Client) Screenshot(ctx context.Context, id string) ([]byte, error) {
	var png []byte
	return png, c.Do(ctx, "GET", "/v1/leases/"+url.PathEscape(id)+"/screenshot", nil, &png)
}

func (c *Client) Logs(ctx context.Context, device string) (string, error) {
	var out struct{ Log string }
	return out.Log, c.Do(ctx, "GET", "/v1/devices/"+url.PathEscape(device)+"/logs", nil, &out)
}

// Acquire queues and blocks until the lease is active and the device ready.
// progress is called on every queue update.
func (c *Client) Acquire(ctx context.Context, device, note string, progress func(Lease)) (Lease, error) {
	l, err := c.Request(ctx, device, note)
	if err != nil {
		return l, err
	}
	for {
		if progress != nil {
			progress(l)
		}
		switch {
		case l.State == "active" && l.Ready:
			return l, nil
		case l.State != "queued" && l.State != "active":
			return l, fmt.Errorf("lease %s ended while waiting: %s", l.ID, l.State)
		}
		if l.State == "active" {
			// Granted but booting (e.g. after a reset). Keep it alive.
			select {
			case <-ctx.Done():
				c.Release(context.Background(), l.ID)
				return l, ctx.Err()
			case <-time.After(3 * time.Second):
			}
			l, err = c.Renew(ctx, l.ID)
		} else {
			l, err = c.Get(ctx, l.ID, 30*time.Second)
		}
		if err != nil {
			if ctx.Err() != nil {
				c.Release(context.Background(), l.ID)
				return l, ctx.Err()
			}
			return l, err
		}
	}
}

// WaitReady renews until the leased device is booted again (after a reset).
func (c *Client) WaitReady(ctx context.Context, id string) (Lease, error) {
	time.Sleep(2 * time.Second) // let the reset take the device down first
	for {
		l, err := c.Renew(ctx, id)
		if err != nil || l.Ready {
			return l, err
		}
		select {
		case <-ctx.Done():
			return l, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// Heartbeat renews a lease every interval until ctx ends. It returns an
// error if the lease is lost.
func (c *Client) Heartbeat(ctx context.Context, id string, every time.Duration) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			l, err := c.Renew(ctx, id)
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				return err
			}
			if l.State != "active" {
				return errors.New("lease lost: " + l.State)
			}
		}
	}
}
