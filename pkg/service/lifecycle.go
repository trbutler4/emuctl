package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/trbutler4/emuctl/pkg/config"
	"github.com/trbutler4/emuctl/pkg/emulator"
	"github.com/trbutler4/emuctl/pkg/lease"
)

// deviceOps are the adb operations the release and health paths use. The
// emulator implements them; tests substitute fakes.
type deviceOps interface {
	Check(ctx context.Context) (emulator.Health, error)
	ClearPackages(ctx context.Context, pkgs []string) error
	DisableAnimations(ctx context.Context) error
	Shell(ctx context.Context, command string) ([]byte, int, error)
}

// maxUnverified inconclusive health checks in a row restart a device.
const maxUnverified = 3

func (s *Service) ops(d *device) deviceOps {
	if s.opsFn != nil {
		return s.opsFn(d.emu.Name)
	}
	return d.emu
}

func (s *Service) booted(d *device) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.st.State == "booted"
}

// onEnd runs when a lease ends. The lease manager holds the device out of
// the pool (HoldOnEnd) until this returns it with Done, so the next holder
// never sees the previous holder's connections, app data or a reset in
// progress.
func (s *Service) onEnd(l lease.Lease) {
	s.Log.Info("lease ended", "lease", l.ID, "user", l.User, "device", l.Device, "state", l.State)
	d := s.devs[l.Device]
	defer s.Leases.Done(l.Device)
	if d.fwd != nil {
		d.fwd.CloseAll()
	}
	if s.Cfg.Release == config.ReleaseWipe {
		s.reset(d, true) // busy until it boots again
		return
	}
	if !s.booted(d) {
		return // the supervisor is already restarting it
	}
	log := s.Log.With("device", l.Device)
	if err := s.releaseClean(d); err != nil {
		log.Warn("release cleanup failed, restarting", "err", err)
		s.markBroken(d, err)
		return
	}
	s.healthGate(d, log, "release")
}

// releaseClean clears configured packages, runs release commands and turns
// animations back off, bounded to two minutes.
func (s *Service) releaseClean(d *device) error {
	ctx, cancel := context.WithTimeout(s.baseCtx(), 2*time.Minute)
	defer cancel()
	o := s.ops(d)
	if s.Cfg.Release == config.ReleaseClear {
		if err := o.ClearPackages(ctx, s.Cfg.ReleasePackages); err != nil {
			return err
		}
	}
	for _, c := range s.Cfg.ReleaseCommands {
		out, code, err := o.Shell(ctx, c)
		if err != nil {
			return fmt.Errorf("release command %q: %w", c, err)
		}
		if code != 0 {
			return fmt.Errorf("release command %q exited %d: %s", c, code, out)
		}
	}
	if *s.Cfg.DisableAnimations {
		if err := o.DisableAnimations(ctx); err != nil {
			return err
		}
	}
	return nil
}

// healthGate checks a device that is out of the pool. Broken devices are
// restarted. A single inconclusive result leaves the device in service: one
// slow adb call is not evidence of a failed device, and restarting a healthy
// one costs a full boot. maxUnverified inconclusive checks in a row are
// treated as broken, since a hung emulator still accepts TCP connections and
// would otherwise stay in the pool forever.
func (s *Service) healthGate(d *device, log *slog.Logger, why string) emulator.Health {
	ctx, cancel := context.WithTimeout(s.baseCtx(), 30*time.Second)
	defer cancel()
	h, err := s.ops(d).Check(ctx)
	switch h {
	case emulator.Broken:
		log.Warn("health check: broken, restarting", "check", why, "err", err)
		s.markBroken(d, err)
	case emulator.Unknown:
		n := 0
		s.set(d, func(st *DeviceStatus) { st.Unverified++; n = st.Unverified })
		if n >= maxUnverified {
			log.Warn("health check: inconclusive too many times in a row, restarting", "check", why, "count", n, "err", err)
			s.markBroken(d, fmt.Errorf("%d inconclusive health checks in a row: %v", n, err))
			return emulator.Broken
		}
		log.Warn("health check: inconclusive, keeping device", "check", why, "count", n, "err", err)
	default:
		s.set(d, func(st *DeviceStatus) { st.Checked, st.Unverified = time.Now(), 0 })
	}
	return h
}

func (s *Service) markBroken(d *device, err error) {
	s.set(d, func(st *DeviceStatus) {
		st.Restarts++
		st.Unverified = 0
		if err != nil {
			st.LastErr = err.Error()
		}
	})
	s.reset(d, false)
}

// healthLoop checks idle booted devices every interval. A device is taken out
// of the pool for the check, so a lease never lands mid-check.
func (s *Service) healthLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, name := range s.Cfg.Names() {
			d := s.devs[name]
			if !s.booted(d) || !s.Leases.Hold(name) {
				continue
			}
			s.healthGate(d, s.Log.With("device", name), "idle")
			s.Leases.Done(name)
		}
	}
}

// prepare runs once after each boot, before the device takes leases.
func (s *Service) prepare(ctx context.Context, d *device, log *slog.Logger) {
	if !*s.Cfg.DisableAnimations {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.ops(d).DisableAnimations(ctx); err != nil {
		log.Warn("disable animations", "err", err)
	}
}

func (s *Service) baseCtx() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}
