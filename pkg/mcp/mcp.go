// Package mcp exposes the emuctl client as a Model Context Protocol server
// over stdio, so agents can queue for, use and release emulators. Leases
// taken through MCP are heartbeated by this process and released when it
// exits, so an agent that forgets to release never holds a device for long.
package mcp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/trbutler4/emuctl/pkg/client"
)

const Instructions = `emuctl: managed Android emulators. Devices are exclusive and queued FIFO across all users and agents.

Workflow:
1. emulator_acquire (optionally device=NAME). Blocks while queued up to wait_seconds; if it returns state "queued", call it again with the same lease_id to keep your place. Never request a second lease while one is queued.
2. When active and ready, use the device:
     emulator_shell runs "adb shell" commands (am start, pm list packages, input tap X Y, input text, uiautomator dump /dev/tty, getprop).
     emulator_screenshot returns the current screen as an image.
     emulator_install installs an APK from this machine.
   A black screen with only the status bar is usually a lock prompt: apps that lock with the device credential show a
   system prompt that adb screenshots blank. emulator_unlock gets past it (fingerprint, else the device PIN), and
   emulator_screenshot with source=host shows what is really on screen.
   With adb or Maestro installed locally you can also drive the returned "adb" target directly:
     adb connect <adb>, then adb -s <adb> ..., or maestro --device <adb> test flow.yaml
   Only the machine that acquired the lease can connect. Other clients are refused.
3. emulator_reset wipes app data and reboots (wipe=true) or just reboots, and returns when the device is ready.
4. Call emulator_release as soon as you are done. Others may be waiting. The lease is kept alive automatically while this MCP server runs and is released if it exits.

Do not run adb kill-server or connect to devices you do not hold. Boot problems: emulator_logs.`

// image is a tool result sent as MCP image content.
type image struct{ png []byte }

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	run         func(ctx context.Context, args map[string]any) (any, error)
}

type Server struct {
	C       *client.Client
	version string
	mu      sync.Mutex
	leases  map[string]context.CancelFunc // heartbeats for leases we hold
	tools   []tool
	out     *json.Encoder
	outMu   sync.Mutex
}

func obj(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any  { return map[string]any{"type": "string", "description": desc} }
func boolean(d string) map[string]any { return map[string]any{"type": "boolean", "description": d} }
func num(d string) map[string]any     { return map[string]any{"type": "integer", "description": d} }

func sarg(a map[string]any, k string) string { s, _ := a[k].(string); return s }

func New(c *client.Client, version string) *Server {
	s := &Server{C: c, version: version, leases: map[string]context.CancelFunc{}}
	s.tools = []tool{
		{"emulator_usage", "How to use the shared emulators. Read this first.", obj(nil),
			func(ctx context.Context, a map[string]any) (any, error) { return Instructions, nil }},
		{"emulator_devices", "List devices, their state and current holder.", obj(nil),
			func(ctx context.Context, a map[string]any) (any, error) { return s.C.Devices(ctx) }},
		{"emulator_queue", "List leases waiting for a device, in order.", obj(nil),
			func(ctx context.Context, a map[string]any) (any, error) { return s.C.Queue(ctx) }},
		{"emulator_acquire", "Queue for a device (or keep waiting on lease_id). Returns the lease; when state is active and ready, use its adb target.",
			obj(map[string]any{
				"device":       str("specific device name; omit for any"),
				"note":         str("what you are doing, shown to others"),
				"lease_id":     str("continue waiting on an earlier queued lease"),
				"wait_seconds": num("max seconds to block (default 120, max 600)"),
			}), s.acquire},
		{"emulator_status", "Current state of one of your leases.", obj(map[string]any{"lease_id": str("lease id")}, "lease_id"),
			func(ctx context.Context, a map[string]any) (any, error) { return s.C.Renew(ctx, sarg(a, "lease_id")) }},
		{"emulator_install", "Install an APK from this machine onto your leased device (adb install -r -g).",
			obj(map[string]any{"lease_id": str("lease id"), "apk_path": str("local APK path")}, "lease_id", "apk_path"),
			func(ctx context.Context, a map[string]any) (any, error) {
				f, err := os.Open(sarg(a, "apk_path"))
				if err != nil {
					return nil, err
				}
				defer f.Close()
				return s.C.Install(ctx, sarg(a, "lease_id"), f)
			}},
		{"emulator_shell", "Run a command on your leased device with adb shell (on the emulator host, so no local adb is needed). Returns output and exit code.",
			obj(map[string]any{"lease_id": str("lease id"), "command": str("shell command, e.g. 'input tap 540 1200' or 'am start -n pkg/.Main'")}, "lease_id", "command"),
			func(ctx context.Context, a map[string]any) (any, error) {
				return s.C.Shell(ctx, sarg(a, "lease_id"), sarg(a, "command"))
			}},
		{"emulator_screenshot", "Screenshot of your leased device, returned as a PNG image. Pass save_path to also write it to a file on this machine. source=host captures the emulator display from outside Android, which shows lock and PIN prompts that the default adb capture returns black.",
			obj(map[string]any{"lease_id": str("lease id"), "save_path": str("optional local file path for the PNG"),
				"source": str("adb (default) or host")}, "lease_id"),
			func(ctx context.Context, a map[string]any) (any, error) {
				shot := s.C.Screenshot
				if sarg(a, "source") == "host" {
					shot = s.C.HostScreenshot
				}
				png, err := shot(ctx, sarg(a, "lease_id"))
				if err != nil {
					return nil, err
				}
				if p := sarg(a, "save_path"); p != "" {
					if err := os.WriteFile(p, png, 0o644); err != nil {
						return nil, err
					}
				}
				return image{png}, nil
			}},
		{"emulator_unlock", "Get your leased device past the lock screen and any app credential prompt (system 'Unlock ...' / BiometricPrompt): touches the enrolled fingerprint, else enters the device PIN. Safe to call when nothing is locked.",
			obj(map[string]any{"lease_id": str("lease id")}, "lease_id"),
			func(ctx context.Context, a map[string]any) (any, error) { return s.C.Unlock(ctx, sarg(a, "lease_id")) }},
		{"emulator_finger", "Touch the emulator's fingerprint sensor (finger 1 is the one emuctl enrolls).",
			obj(map[string]any{"lease_id": str("lease id"), "finger": num("finger id, default 1")}, "lease_id"),
			func(ctx context.Context, a map[string]any) (any, error) {
				f := 1
				if v, ok := a["finger"].(float64); ok {
					f = int(v)
				}
				return map[string]int{"touched": f}, s.C.Finger(ctx, sarg(a, "lease_id"), f)
			}},
		{"emulator_reset", "Reboot your leased device, wiping all data when wipe=true. Blocks until ready.",
			obj(map[string]any{"lease_id": str("lease id"), "wipe": boolean("wipe user data")}, "lease_id"),
			func(ctx context.Context, a map[string]any) (any, error) {
				wipe, _ := a["wipe"].(bool)
				if _, err := s.C.Reset(ctx, sarg(a, "lease_id"), wipe); err != nil {
					return nil, err
				}
				ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
				defer cancel()
				return s.C.WaitReady(ctx, sarg(a, "lease_id"))
			}},
		{"emulator_release", "Release your lease (or leave the queue). Do this as soon as you are done.",
			obj(map[string]any{"lease_id": str("lease id")}, "lease_id"),
			func(ctx context.Context, a map[string]any) (any, error) {
				id := sarg(a, "lease_id")
				s.stopHeartbeat(id)
				return map[string]string{"released": id}, s.C.Release(ctx, id)
			}},
		{"emulator_logs", "Tail of a device's emulator log.", obj(map[string]any{"device": str("device name")}, "device"),
			func(ctx context.Context, a map[string]any) (any, error) { return s.C.Logs(ctx, sarg(a, "device")) }},
	}
	return s
}

func (s *Server) heartbeat(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.leases[id]; ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.leases[id] = cancel
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				l, err := s.C.Renew(ctx, id)
				if ctx.Err() == nil && (err != nil || (l.State != "active" && l.State != "queued")) {
					s.stopHeartbeat(id)
					return
				}
			}
		}
	}()
}

func (s *Server) stopHeartbeat(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.leases[id]; ok {
		c()
		delete(s.leases, id)
	}
}

func (s *Server) acquire(ctx context.Context, a map[string]any) (any, error) {
	wait := 120 * time.Second
	if w, ok := a["wait_seconds"].(float64); ok && w >= 0 {
		wait = min(time.Duration(w)*time.Second, 10*time.Minute)
	}
	var l client.Lease
	var err error
	if id := sarg(a, "lease_id"); id != "" {
		l, err = s.C.Renew(ctx, id)
	} else {
		l, err = s.C.Request(ctx, sarg(a, "device"), sarg(a, "note"))
	}
	if err != nil {
		return nil, err
	}
	s.heartbeat(l.ID) // keeps our queue place and the lease alive
	deadline := time.Now().Add(wait)
	for {
		if l.State == "active" && l.Ready {
			if l.ADB == "" {
				return map[string]any{"lease": l, "next": "Use emulator_shell, emulator_screenshot and emulator_install with this lease_id. Release with emulator_release when done."}, nil
			}
			if ll, err := s.C.LocalADB(ctx, l); err == nil {
				l = ll
			}
			return map[string]any{"lease": l, "next": fmt.Sprintf(
				"Use emulator_shell, emulator_screenshot and emulator_install with this lease_id. With local adb/Maestro: adb connect %s, then serial %s (maestro --device %s). Release with emulator_release when done.", l.ADB, l.Serial, l.Serial)}, nil
		}
		if l.State != "queued" && l.State != "active" {
			s.stopHeartbeat(l.ID)
			return map[string]any{"lease": l, "next": "lease ended, call emulator_acquire again"}, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return map[string]any{"lease": l, "next": fmt.Sprintf(
				"still %s (position %d). Call emulator_acquire with lease_id=%s to keep waiting.", l.State, l.Position, l.ID)}, nil
		}
		if l.State == "queued" {
			l, err = s.C.Get(ctx, l.ID, min(left, 30*time.Second))
		} else {
			time.Sleep(min(left, 3*time.Second))
			l, err = s.C.Renew(ctx, l.ID)
		}
		if err != nil {
			return nil, err
		}
	}
}

func (s *Server) send(r rpcResp) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	r.JSONRPC = "2.0"
	s.out.Encode(r)
}

// Serve speaks newline-delimited JSON-RPC on in/out until in closes, then
// releases every lease it still holds.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = json.NewEncoder(out)
	defer s.releaseAll()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var wg sync.WaitGroup
	defer wg.Wait()
	for sc.Scan() {
		var req rpcReq
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			s.send(rpcResp{ID: json.RawMessage("null"), Error: &rpcErr{-32700, "parse error"}})
			continue
		}
		if req.ID == nil {
			continue // notification
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handle(ctx, req)
		}()
	}
	return sc.Err()
}

func (s *Server) releaseAll() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.leases))
	for id, cancel := range s.leases {
		cancel()
		ids = append(ids, id)
	}
	s.leases = map[string]context.CancelFunc{}
	s.mu.Unlock()
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.C.Release(ctx, id)
		cancel()
	}
}

func (s *Server) handle(ctx context.Context, req rpcReq) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		s.send(rpcResp{ID: req.ID, Result: map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "emuctl", "version": s.version},
			"instructions":    Instructions,
		}})
	case "ping":
		s.send(rpcResp{ID: req.ID, Result: map[string]any{}})
	case "tools/list":
		s.send(rpcResp{ID: req.ID, Result: map[string]any{"tools": s.tools}})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		json.Unmarshal(req.Params, &p)
		for _, t := range s.tools {
			if t.Name != p.Name {
				continue
			}
			v, err := t.run(ctx, p.Arguments)
			if img, ok := v.(image); ok && err == nil {
				s.send(rpcResp{ID: req.ID, Result: map[string]any{"content": []map[string]any{{
					"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(img.png)}}}})
				return
			}
			text, isErr := "", false
			if err != nil {
				text, isErr = err.Error(), true
			} else if str, ok := v.(string); ok {
				text = str
			} else {
				b, _ := json.MarshalIndent(v, "", "  ")
				text = string(b)
			}
			s.send(rpcResp{ID: req.ID, Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}})
			return
		}
		s.send(rpcResp{ID: req.ID, Error: &rpcErr{-32602, "unknown tool " + p.Name}})
	default:
		s.send(rpcResp{ID: req.ID, Error: &rpcErr{-32601, "method not found: " + req.Method}})
	}
}
