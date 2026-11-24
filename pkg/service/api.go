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

	"github.com/trbutler4/emuctl/pkg/apkstore"
	"github.com/trbutler4/emuctl/pkg/emulator"
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
//	POST   /v1/leases/{id}/install?apk=KEY   install a stored APK; no-op if the device has it
//	GET    /v1/apks                          stored APKs
//	PUT    /v1/apks/{key}                    body: APK bytes; 409 if key exists
//	POST   /v1/leases/{id}/reset?wipe=1      reboot the leased device, optionally wiping data
//	POST   /v1/leases/{id}/shell {"command"} adb shell on the leased device
//	GET    /v1/leases/{id}/screenshot        PNG of the screen (?source=host: emulator framebuffer, shows secure windows)
//	POST   /v1/leases/{id}/unlock            get past the lock screen and app credential prompts
//	POST   /v1/leases/{id}/finger?id=1       touch the virtual fingerprint sensor
//	GET    /v1/devices/{name}/logs           tail of the emulator log
//	GET    /metrics                          Prometheus metrics (or on metrics_addr)
//	POST   /v1/devices/{name}/restart        force restart a device that is free or leased by you
func (s *Service) serveAPI(ctx context.Context) error {
	srv := &http.Server{Addr: s.Cfg.API, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	if s.Cfg.MetricsAddr != "" && s.Cfg.MetricsAddr != s.Cfg.API {
		msrv := &http.Server{Addr: s.Cfg.MetricsAddr, Handler: s.metricsMux(), ReadHeaderTimeout: 10 * time.Second}
		go func() { <-ctx.Done(); msrv.Close() }()
		go func() {
			s.Log.Info("metrics listening", "addr", s.Cfg.MetricsAddr)
			if err := msrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.Log.Error("metrics server", "err", err)
			}
		}()
	}
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
		s.Metrics.restarts.Inc(l.Device, "request")
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
		if r.URL.Query().Get("source") == "host" {
			png, err = d.emu.HostScreenshot(ctx)
		}
		if err != nil {
			return nil, fail(http.StatusBadGateway, "screencap: %v", err)
		}
		return raw{"image/png", png}, nil
	}))
	mux.HandleFunc("POST /v1/leases/{id}/unlock", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		l, err := s.owned(r, user, true)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		d := s.devs[l.Device]
		if err := d.emu.Connect(ctx); err != nil {
			return nil, fail(http.StatusServiceUnavailable, "device not reachable: %v", err)
		}
		res := d.emu.UnlockScreen(ctx)
		s.Log.Info("unlock", "lease", l.ID, "user", user, "device", l.Device, "found", res.Found,
			"actions", res.Actions, "cleared", res.Cleared, "err", res.Error)
		return res, nil
	}))
	mux.HandleFunc("POST /v1/leases/{id}/finger", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		l, err := s.owned(r, user, true)
		if err != nil {
			return nil, err
		}
		id := emulator.FingerID
		if v := r.URL.Query().Get("id"); v != "" {
			if id, err = strconv.Atoi(v); err != nil || id < 1 || id > 10 {
				return nil, fail(http.StatusBadRequest, "id must be a finger id 1-10")
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := s.devs[l.Device].emu.FingerTouch(ctx, id); err != nil {
			return nil, fail(http.StatusBadGateway, "%v", err)
		}
		return map[string]any{"touched": id}, nil
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
		s.Metrics.restarts.Inc(d.emu.Name, "request")
		go s.reset(d, r.URL.Query().Get("wipe") == "1")
		return map[string]string{"restarting": d.emu.Name}, nil
	}))
	mux.HandleFunc("GET /v1/apks", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		return s.APKs.List()
	}))
	mux.HandleFunc("PUT /v1/apks/{key}", s.wrap(func(w http.ResponseWriter, r *http.Request, user string) (any, error) {
		return s.putAPK(r, user)
	}))
	if s.Cfg.MetricsAddr == "" || s.Cfg.MetricsAddr == s.Cfg.API {
		// Same listener as the API: /metrics needs a token unless the API
		// is open, like every other endpoint.
		mux.Handle("GET /metrics", s.metricsAuth(s.Metrics.Handler()))
	}
	return s.Metrics.instrument(mux)
}

// metricsMux serves /metrics on a separate metrics_addr, without a token.
func (s *Service) metricsMux() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", s.Metrics.Handler())
	return mux
}

// metricsAuth requires an API token on the shared listener.
func (s *Service) metricsAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, ok := s.Cfg.Authenticate(token); !ok && !s.Cfg.Open() {
			http.Error(w, "missing or invalid token", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

const maxAPK = apkstore.MaxSize

// install handles POST /v1/leases/{id}/install. With ?apk=KEY it installs a
// stored APK, skipping the install when the device already has that key.
// Otherwise the body is an APK to install once, which is not stored.
func (s *Service) install(r *http.Request, l lease.Lease) (any, error) {
	if key := r.URL.Query().Get("apk"); key != "" {
		return s.installStored(r, l, key)
	}
	d := s.devs[l.Device]
	if err := os.MkdirAll(s.Cfg.DeviceDir(l.Device), 0o700); err != nil {
		return nil, err
	}
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
	// The device no longer matches any stored key.
	s.set(d, func(st *DeviceStatus) { st.APK = "" })
	out, err := s.adbInstall(r.Context(), l, path, "upload")
	if err != nil {
		return nil, err
	}
	return map[string]any{"installed": true, "bytes": n, "output": out}, nil
}

func (s *Service) installStored(r *http.Request, l lease.Lease, key string) (any, error) {
	path, err := s.APKs.Path(key)
	switch {
	case errors.Is(err, apkstore.ErrBadKey):
		return nil, fail(http.StatusBadRequest, "%v", err)
	case errors.Is(err, apkstore.ErrNotFound):
		return nil, fail(http.StatusNotFound, "apk %q not in store, upload it with PUT /v1/apks/%s", key, key)
	case err != nil:
		return nil, err
	}
	d := s.devs[l.Device]
	d.mu.Lock()
	have := d.st.APK
	d.mu.Unlock()
	s.APKs.Touch(key)
	if have == key && r.URL.Query().Get("force") != "1" {
		s.Metrics.installs.Inc(l.Device, "store", "skipped")
		return map[string]any{"installed": false, "apk": key, "output": "already installed"}, nil
	}
	s.set(d, func(st *DeviceStatus) { st.APK = "" })
	out, err := s.adbInstall(r.Context(), l, path, "store")
	if err != nil {
		return nil, err
	}
	s.set(d, func(st *DeviceStatus) { st.APK = key })
	return map[string]any{"installed": true, "apk": key, "output": out}, nil
}

// adbInstall runs `adb install -r -g` on the leased device. Tests replace it.
func (s *Service) adbInstall(ctx context.Context, l lease.Lease, path, source string) (out string, err error) {
	start := time.Now()
	defer func() {
		s.Metrics.installDuration.Observe(time.Since(start).Seconds(), l.Device, source)
		result := "installed"
		if err != nil {
			result = "failed"
		}
		s.Metrics.installs.Inc(l.Device, source, result)
	}()
	if s.installFn != nil {
		return s.installFn(ctx, l.Device, path)
	}
	d := s.devs[l.Device]
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := d.emu.Connect(ctx); err != nil {
		return "", fail(http.StatusServiceUnavailable, "device not reachable: %v", err)
	}
	raw, err := d.emu.ADB(ctx, "install", "-r", "-g", path)
	s.Log.Info("install", "lease", l.ID, "user", l.User, "device", l.Device, "apk", path, "err", err)
	if err != nil || !strings.Contains(string(raw), "Success") {
		return "", fail(http.StatusUnprocessableEntity, "adb install failed: %v %s", err, strings.TrimSpace(string(raw)))
	}
	return strings.TrimSpace(string(raw)), nil
}

func (s *Service) putAPK(r *http.Request, user string) (any, error) {
	key := r.PathValue("key")
	n, err := s.APKs.Put(key, r.Body)
	result := "stored"
	switch {
	case errors.Is(err, apkstore.ErrExists):
		result = "exists"
	case err != nil:
		result = "rejected"
	}
	s.Metrics.apkUploads.Inc(result)
	switch {
	case errors.Is(err, apkstore.ErrBadKey):
		return nil, fail(http.StatusBadRequest, "%v", err)
	case errors.Is(err, apkstore.ErrExists):
		return nil, fail(http.StatusConflict, "apk %q already stored, entries are immutable", key)
	case errors.Is(err, apkstore.ErrTooLarge):
		return nil, fail(http.StatusRequestEntityTooLarge, "%v", err)
	case err != nil:
		return nil, err
	}
	s.Log.Info("apk stored", "key", key, "user", user, "bytes", n)
	return map[string]any{"key": key, "bytes": n}, nil
}
