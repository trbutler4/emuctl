// Package service runs the emulators, the lease queue, the per-device adb
// forwarders and the HTTP API.
package service

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/trbutler4/emuctl/pkg/apkstore"
	"github.com/trbutler4/emuctl/pkg/config"
	"github.com/trbutler4/emuctl/pkg/emulator"
	"github.com/trbutler4/emuctl/pkg/lease"
	"github.com/trbutler4/emuctl/pkg/proxy"
)

// DeviceStatus is reported per device by GET /v1/devices.
type DeviceStatus struct {
	Name     string       `json:"name"`
	State    string       `json:"state"` // stopped | starting | booted | crashed | resetting
	Serial   string       `json:"serial"`
	ADB      string       `json:"adb,omitempty"` // host:port for `adb connect`, when exposed
	Profile  string       `json:"profile"`
	Since    time.Time    `json:"since"`
	Restarts int          `json:"restarts"`
	LastErr  string       `json:"last_error,omitempty"`
	APK      string       `json:"apk,omitempty"` // store key last installed by emuctl; cleared by wipe or ad hoc install
	Holder   *lease.Lease `json:"holder,omitempty"`
}

type device struct {
	emu  *emulator.Emulator
	fwd  *proxy.Forwarder
	mu   sync.Mutex
	st   DeviceStatus
	stop context.CancelFunc
	done chan struct{}
	wipe bool
}

type Service struct {
	Cfg    *config.Config
	Log    *slog.Logger
	Leases *lease.Manager
	APKs   *apkstore.Store
	devs   map[string]*device
	ctx    context.Context

	// installFn replaces adb install in tests.
	installFn func(ctx context.Context, device, path string) (string, error)
}

func New(c *config.Config, log *slog.Logger) (*Service, error) {
	s := &Service{Cfg: c, Log: log, devs: map[string]*device{},
		APKs: &apkstore.Store{Dir: filepath.Join(c.StateDir, "apks")}}
	s.Leases = lease.NewManager(c.Names(), c.TTL())
	s.Leases.Hooks = lease.Hooks{Granted: s.onGrant, Ended: s.onEnd}
	for _, name := range c.Names() {
		e, err := emulator.New(c, name)
		if err != nil {
			return nil, err
		}
		d := &device{emu: e, st: DeviceStatus{Name: name, State: "stopped", Serial: e.Serial(),
			Profile: c.Devices[name].Profile, Since: time.Now()}}
		if p := c.Devices[name].ExposePort; p != 0 {
			d.st.ADB = net.JoinHostPort(c.Listen, strconv.Itoa(p))
			d.fwd = &proxy.Forwarder{Listen: d.st.ADB, Target: fmt.Sprintf("127.0.0.1:%d", e.ADBPort()),
				Log: log.With("device", name), Allow: s.allow(name)}
		}
		s.devs[name] = d
		// Nothing is granted until the device has booted.
		s.Leases.SetBusy(name, true)
	}
	return s, nil
}

// allow admits only the client IP of the device's current lease holder.
func (s *Service) allow(name string) func(string) bool {
	return func(ip string) bool {
		l, ok := s.Leases.Holder(name)
		return ok && sameIP(l.ClientIP, ip)
	}
}

func sameIP(a, b string) bool {
	pa, pb := net.ParseIP(a), net.ParseIP(b)
	return pa != nil && pb != nil && pa.Equal(pb)
}

func (s *Service) onGrant(l lease.Lease) {
	s.Log.Info("lease granted", "lease", l.ID, "user", l.User, "device", l.Device, "client", l.ClientIP)
}

func (s *Service) onEnd(l lease.Lease) {
	s.Log.Info("lease ended", "lease", l.ID, "user", l.User, "device", l.Device, "state", l.State)
	d := s.devs[l.Device]
	if d.fwd != nil {
		d.fwd.CloseAll()
	}
	if s.Cfg.ResetOnRelease {
		s.reset(d, true)
	}
}

// Run starts every device, the forwarders and the lease sweeper, then
// serves the API until ctx is done.
func (s *Service) Run(ctx context.Context) error {
	s.ctx = ctx
	var wg sync.WaitGroup
	for _, name := range s.Cfg.Names() {
		d := s.devs[name]
		if d.fwd != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := d.fwd.Serve(ctx); err != nil {
					s.Log.Error("forwarder", "device", name, "listen", d.fwd.Listen, "err", err)
				}
			}()
		}
		s.start(d)
	}
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Leases.Sweep()
			}
		}
	}()
	err := s.serveAPI(ctx)
	for _, d := range s.devs {
		s.halt(d)
	}
	wg.Wait()
	return err
}

func (s *Service) set(d *device, f func(*DeviceStatus)) {
	d.mu.Lock()
	f(&d.st)
	d.mu.Unlock()
}

func (s *Service) start(d *device) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stop != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	d.stop, d.done = cancel, make(chan struct{})
	go func() {
		defer close(d.done)
		s.supervise(ctx, d)
	}()
}

func (s *Service) halt(d *device) {
	d.mu.Lock()
	cancel, done := d.stop, d.done
	d.stop = nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	s.Leases.SetBusy(d.emu.Name, true)
	s.set(d, func(st *DeviceStatus) { st.State, st.Since = "stopped", time.Now() })
}

// reset restarts the emulator, wiping user data when wipe is set. The device
// takes no new leases until it boots again.
func (s *Service) reset(d *device, wipe bool) {
	s.halt(d)
	d.mu.Lock()
	d.wipe = wipe
	d.mu.Unlock()
	if wipe {
		s.set(d, func(st *DeviceStatus) { st.State, st.APK = "resetting", "" })
	}
	s.start(d)
}

func (s *Service) supervise(ctx context.Context, d *device) {
	log := s.Log.With("device", d.emu.Name)
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := s.runOnce(ctx, d, log)
		s.Leases.SetBusy(d.emu.Name, true)
		if ctx.Err() != nil {
			return
		}
		log.Warn("emulator exited", "err", err)
		s.set(d, func(st *DeviceStatus) {
			st.State, st.Since, st.Restarts = "crashed", time.Now(), st.Restarts+1
			if err != nil {
				st.LastErr = err.Error()
			}
		})
		if time.Since(started) > 5*time.Minute {
			backoff = 5 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

func (s *Service) runOnce(ctx context.Context, d *device, log *slog.Logger) error {
	if err := d.emu.Create(ctx); err != nil {
		return err
	}
	cmd, logf, err := d.emu.Command(ctx)
	if err != nil {
		return err
	}
	defer logf.Close()
	d.mu.Lock()
	if d.wipe {
		cmd.Args = append(cmd.Args, "-wipe-data")
		d.wipe = false
	}
	d.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Info("emulator started", "pid", cmd.Process.Pid, "serial", d.emu.Serial())
	s.set(d, func(st *DeviceStatus) {
		if st.State != "resetting" {
			st.State = "starting"
		}
		st.Since, st.LastErr = time.Now(), ""
	})
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	bootCtx, cancel := context.WithTimeout(ctx, s.Cfg.BootTimeoutDuration())
	defer cancel()
	booted := make(chan error, 1)
	go func() { booted <- d.emu.WaitBooted(bootCtx) }()
	for {
		select {
		case err := <-exited:
			return err
		case err := <-booted:
			booted = nil
			if err != nil {
				log.Error("boot timeout, killing", "err", err)
				cmd.Process.Kill()
				continue
			}
			log.Info("booted")
			s.set(d, func(st *DeviceStatus) { st.State, st.Since = "booted", time.Now() })
			s.Leases.SetBusy(d.emu.Name, false)
		}
	}
}

func (s *Service) Devices() []DeviceStatus {
	out := []DeviceStatus{}
	for _, name := range s.Cfg.Names() {
		d := s.devs[name]
		d.mu.Lock()
		st := d.st
		d.mu.Unlock()
		if l, ok := s.Leases.Holder(name); ok {
			st.Holder = &l
		}
		out = append(out, st)
	}
	return out
}
