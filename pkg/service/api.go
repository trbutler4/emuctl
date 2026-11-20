package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/trbutler4/emuctl/pkg/lease"
)

// API, all JSON, bearer-token auth:
//
//	GET    /v1/devices                       device states and holders
//	GET    /v1/queue                         queued leases in order
//	POST   /v1/leases {"device","note","ttl_seconds"} enqueue; device "" = any
//	GET    /v1/leases/{id}?wait=30s          long-poll until granted/changed
//	POST   /v1/leases/{id}/renew             heartbeat, extends TTL
//	DELETE /v1/leases/{id}                   release (or leave the queue)
//	POST   /v1/leases/{id}/install           body: APK bytes, adb install -r -g
//	POST   /v1/leases/{id}/reset?wipe=1      reboot the leased device, optionally wiping data
//	POST   /v1/leases/{id}/shell {"command"} adb shell on the leased device
//	GET    /v1/leases/{id}/screenshot        PNG of the screen
//	GET    /v1/devices/{name}/logs           tail of the emulator log
//	POST   /v1/devices/{name}/restart        admin: force restart (ends lease)
func (s *Service) serveAPI(ctx context.Context) error {
	srv := &http.Server{Addr: s.Cfg.API, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	s.Log.Info("api listening", "addr", s.Cfg.API, "devices", s.Cfg.Names())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type httpError struct {
	code int
	msg  string
}

func (e httpError) Error() string { return e.msg }

func fail(code int, f string, a ...any) error { return httpError{code, fmt.Sprintf(f, a...)} }

type handler func(w http.ResponseWriter, r *http.Request, user string) (any, error)

func (s *Service) wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		user, ok := s.Cfg.Authenticate(token)
		if !ok && s.Cfg.Open() {
			// No users: loopback-only, enforced by config validation.
			user, ok = "local", true
		}
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "missing or invalid token"})
			return
		}
		v, err := h(w, r, user)
		if err != nil {
			code := http.StatusBadRequest
			var he httpError
			if errors.As(err, &he) {
				code = he.code
			} else if errors.Is(err, lease.ErrNotFound) {
				code = http.StatusNotFound
			}
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if b, ok := v.(raw); ok {
			w.Header().Set("Content-Type", b.contentType)
			w.Write(b.body)
			return
		}
		json.NewEncoder(w).Encode(v)
	}
}

// raw is a non-JSON response body.
type raw struct {
	contentType string
	body        []byte
}

func clientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// owned loads a lease and checks it belongs to user.
func (s *Service) owned(r *http.Request, user string, active bool) (lease.Lease, error) {
	l, err := s.Leases.Get(r.PathValue("id"), 0)
	if err != nil {
		return l, err
	}
	if l.User != user {
		return l, fail(http.StatusForbidden, "lease belongs to %s", l.User)
	}
	if active && l.State != lease.Active {
		return l, fail(http.StatusConflict, "lease is %s, not active", l.State)
	}
	return l, nil
}

// LeaseView adds connection details to an active lease.
type LeaseView struct {
	lease.Lease
	ADB    string `json:"adb,omitempty"`    // `adb connect` target
	Serial string `json:"serial,omitempty"` // serial after connect, for maestro --device
	Ready  bool   `json:"ready"`            // device booted and usable
}

// view adds connection details. When the forwarders bind a wildcard address
// the adb target uses the host name the client reached the API through.
func (s *Service) view(r *http.Request, l lease.Lease) LeaseView {
	v := LeaseView{Lease: l}
	if l.State == lease.Active {
		d := s.devs[l.Device]
		d.mu.Lock()
		v.ADB, v.Ready = d.st.ADB, d.st.State == "booted"
		d.mu.Unlock()
		if host, port, err := net.SplitHostPort(v.ADB); err == nil {
			if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
				reqHost := r.Host
				if h, _, err := net.SplitHostPort(r.Host); err == nil {
					reqHost = h
				}
				v.ADB = net.JoinHostPort(reqHost, port)
			}
		}
		v.Serial = v.ADB
	}
	return v
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/devices", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		return s.Devices(), nil
	}))
	mux.HandleFunc("GET /v1/queue", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		return s.Leases.Queue(), nil
	}))
	mux.HandleFunc("POST /v1/leases", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		var req struct {
			Device string `json:"device"`
			Note   string `json:"note"`
			TTL    int    `json:"ttl_seconds"` // without heartbeat; default lease_ttl
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
				return nil, err
			}
		}
		ttl := time.Duration(req.TTL) * time.Second
		if ttl > s.Cfg.MaxTTL() {
			return nil, fail(http.StatusBadRequest, "ttl_seconds above max_lease_ttl %s", s.Cfg.MaxTTL())
		}
		l, err := s.Leases.RequestTTL(user, req.Device, clientIP(r), req.Note, ttl)
		if err != nil {
			return nil, err
		}
		return s.view(r, l), nil
	}))
	mux.HandleFunc("GET /v1/leases/{id}", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		if _, err := s.owned(r, user, false); err != nil {
			return nil, err
		}
		wait, _ := time.ParseDuration(r.URL.Query().Get("wait"))
		l, err := s.Leases.Get(r.PathValue("id"), min(wait, 60*time.Second))
		if err != nil {
			return nil, err
		}
		return s.view(r, l), nil
	}))
	mux.HandleFunc("POST /v1/leases/{id}/renew", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		if _, err := s.owned(r, user, false); err != nil {
			return nil, err
		}
		l, err := s.Leases.Renew(r.PathValue("id"))
		if err != nil {
			return nil, err
		}
		return s.view(r, l), nil
	}))
	mux.HandleFunc("DELETE /v1/leases/{id}", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		if _, err := s.owned(r, user, false); err != nil {
			return nil, err
		}
		l, err := s.Leases.Release(r.PathValue("id"))
		if err != nil {
			return nil, err
		}
		return s.view(r, l), nil
	}))
	mux.HandleFunc("POST /v1/leases/{id}/install", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		l, err := s.owned(r, user, true)
		if err != nil {
			return nil, err
		}
		return s.install(r, l)
	}))
	mux.HandleFunc("POST /v1/leases/{id}/reset", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		l, err := s.owned(r, user, true)
		if err != nil {
			return nil, err
		}
		d := s.devs[l.Device]
		if d.fwd != nil {
			d.fwd.CloseAll()
		}
		wipe := r.URL.Query().Get("wipe") == "1"
		go s.reset(d, wipe)
		s.Log.Info("reset requested", "lease", l.ID, "user", user, "device", l.Device, "wipe", wipe)
		return s.view(r, l), nil
	}))
	mux.HandleFunc("POST /v1/leases/{id}/shell", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		l, err := s.owned(r, user, true)
		if err != nil {
			return nil, err
		}
		var req struct {
			Command string `json:"command"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil || strings.TrimSpace(req.Command) == "" {
			return nil, fail(http.StatusBadRequest, "body must be {\"command\": \"...\"}")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		d := s.devs[l.Device]
		if err := d.emu.Connect(ctx); err != nil {
			return nil, fail(http.StatusServiceUnavailable, "device not reachable: %v", err)
		}
		out, code, err := d.emu.Shell(ctx, req.Command)
		if err != nil {
			return nil, fail(http.StatusBadGateway, "adb shell: %v", err)
		}
		if len(out) > 1<<20 {
			out = append(out[:1<<20], []byte("\n[truncated]")...)
		}
		return map[string]any{"output": string(out), "exit_code": code}, nil
	}))
	mux.HandleFunc("GET /v1/leases/{id}/screenshot", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		l, err := s.owned(r, user, true)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		d := s.devs[l.Device]
		if err := d.emu.Connect(ctx); err != nil {
			return nil, fail(http.StatusServiceUnavailable, "device not reachable: %v", err)
		}
		png, err := d.emu.Screenshot(ctx)
		if err != nil {
			return nil, fail(http.StatusBadGateway, "screencap: %v", err)
		}
		return raw{"image/png", png}, nil
	}))
	mux.HandleFunc("GET /v1/devices/{name}/logs", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		name := r.PathValue("name")
		if _, ok := s.devs[name]; !ok {
			return nil, fail(http.StatusNotFound, "unknown device %q", name)
		}
		b, err := os.ReadFile(s.Cfg.LogPath(name))
		if err != nil {
			return nil, err
		}
		n, _ := strconv.Atoi(r.URL.Query().Get("bytes"))
		if n <= 0 || n > 1<<20 {
			n = 16 << 10
		}
		if len(b) > n {
			b = b[len(b)-n:]
		}
		return map[string]string{"log": string(b)}, nil
	}))
	mux.HandleFunc("POST /v1/devices/{name}/restart", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		d, ok := s.devs[r.PathValue("name")]
		if !ok {
			return nil, fail(http.StatusNotFound, "unknown device")
		}
		if l, held := s.Leases.Holder(d.emu.Name); held && l.User != user {
			return nil, fail(http.StatusConflict, "device is leased by %s", l.User)
		}
		go s.reset(d, r.URL.Query().Get("wipe") == "1")
		return map[string]string{"restarting": d.emu.Name}, nil
	}))
	return mux
}

const maxAPK = 1 << 30

func (s *Service) install(r *http.Request, l lease.Lease) (any, error) {
	d := s.devs[l.Device]
	dir, err := os.MkdirTemp(s.Cfg.DeviceDir(l.Device), "upload-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "app.apk")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(f, io.LimitReader(r.Body, maxAPK+1))
	f.Close()
	if err != nil {
		return nil, err
	}
	if n == 0 || n > maxAPK {
		return nil, fail(http.StatusRequestEntityTooLarge, "APK must be 1 byte to 1 GiB")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if err := d.emu.Connect(ctx); err != nil {
		return nil, fail(http.StatusServiceUnavailable, "device not reachable: %v", err)
	}
	out, err := d.emu.ADB(ctx, "install", "-r", "-g", path)
	s.Log.Info("install", "lease", l.ID, "user", l.User, "device", l.Device, "bytes", n, "err", err)
	if err != nil || !strings.Contains(string(out), "Success") {
		return nil, fail(http.StatusUnprocessableEntity, "adb install failed: %v %s", err, strings.TrimSpace(string(out)))
	}
	return map[string]any{"installed": true, "bytes": n, "output": strings.TrimSpace(string(out))}, nil
}
