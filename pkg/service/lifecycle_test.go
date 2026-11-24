package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trbutler4/emuctl/pkg/config"
	"github.com/trbutler4/emuctl/pkg/emulator"
	"github.com/trbutler4/emuctl/pkg/lease"
)

type fakeOps struct {
	mu       sync.Mutex
	calls    []string
	health   emulator.Health
	clearErr error
	gate     chan struct{} // if set, ClearPackages blocks until closed
}

func (f *fakeOps) log(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *fakeOps) got() string  { f.mu.Lock(); defer f.mu.Unlock(); return strings.Join(f.calls, ",") }

func (f *fakeOps) Check(ctx context.Context) (emulator.Health, error) {
	f.log("check")
	return f.health, errors.New(f.health.String())
}
func (f *fakeOps) ClearPackages(ctx context.Context, pkgs []string) error {
	if f.gate != nil {
		<-f.gate
	}
	f.log("clear:" + strings.Join(pkgs, "+"))
	return f.clearErr
}
func (f *fakeOps) DisableAnimations(ctx context.Context) error { f.log("anim"); return nil }
func (f *fakeOps) KeepAwake(ctx context.Context) error         { f.log("awake"); return nil }
func (f *fakeOps) EnsurePIN(ctx context.Context) error         { f.log("pin"); return nil }
func (f *fakeOps) EnsureFingerprint(ctx context.Context) error { f.log("finger"); return nil }
func (f *fakeOps) Shell(ctx context.Context, c string) ([]byte, int, error) {
	f.log("sh:" + c)
	return nil, 0, nil
}

func lifecycleService(t *testing.T, extra string) (*Service, *fakeOps) {
	cfg, err := config.Parse([]byte(`{"sdk":"/nonexistent","state_dir":"` + t.TempDir() + `",` + extra + `
	  "devices":{"p1":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOps{}
	s.opsFn = func(string) deviceOps { return f }
	s.devs["p1"].st.State = "booted"
	s.Leases.SetBusy("p1", false)
	return s, f
}

func waitState(t *testing.T, m *lease.Manager, id string, want lease.State) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if g, _ := m.Get(id, 0); g.State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	g, _ := m.Get(id, 0)
	t.Fatalf("lease %s: state %s, want %s", id, g.State, want)
}

func TestReleaseClearRunsBeforeNextGrant(t *testing.T) {
	s, f := lifecycleService(t, `"release":"clear","release_packages":["com.example.app"],"release_commands":["rm -f /data/local/tmp/x"],`)
	f.gate = make(chan struct{})
	l1, _ := s.Leases.Request("u1", "", "", "")
	l2, _ := s.Leases.Request("u2", "", "", "")
	s.Leases.Release(l1.ID)
	time.Sleep(50 * time.Millisecond)
	if g, _ := s.Leases.Get(l2.ID, 0); g.State != lease.Queued {
		t.Fatal("next lease granted while release cleanup is still running")
	}
	close(f.gate)
	waitState(t, s.Leases, l2.ID, lease.Active)
	if got, want := f.got(), "clear:com.example.app,sh:rm -f /data/local/tmp/x,anim,check"; got != want {
		t.Fatalf("release ops %q, want %q", got, want)
	}
}

func TestUnknownHealthKeepsDevice(t *testing.T) {
	s, f := lifecycleService(t, "")
	f.health = emulator.Unknown
	l1, _ := s.Leases.Request("u1", "", "", "")
	l2, _ := s.Leases.Request("u2", "", "", "")
	s.Leases.Release(l1.ID)
	waitState(t, s.Leases, l2.ID, lease.Active)
	st := s.Devices()[0]
	if st.State != "booted" || st.Restarts != 0 || st.Unverified != 1 {
		t.Fatalf("inconclusive check must not restart the device: %+v", st)
	}
}

func TestRepeatedUnknownRestarts(t *testing.T) {
	s, f := lifecycleService(t, "")
	f.health = emulator.Unknown
	s.ctx = context.Background()
	d := s.devs["p1"]
	for i := 1; i < maxUnverified; i++ {
		s.healthGate(d, s.Log, "idle")
		if st := s.Devices()[0]; st.Restarts != 0 || st.Unverified != i {
			t.Fatalf("check %d: %+v", i, st)
		}
	}
	if h := s.healthGate(d, s.Log, "idle"); h != emulator.Broken {
		t.Fatalf("check %d: %v, want broken", maxUnverified, h)
	}
	if st := s.Devices()[0]; st.Restarts < 1 {
		t.Fatalf("hung device must be restarted: %+v", st)
	}
	d.ctl.Lock()
	s.halt(d)
	d.ctl.Unlock()
}

func TestBrokenHealthRestarts(t *testing.T) {
	s, f := lifecycleService(t, "")
	f.health = emulator.Broken
	s.ctx = context.Background()
	l1, _ := s.Leases.Request("u1", "", "", "")
	l2, _ := s.Leases.Request("u2", "", "", "")
	s.Leases.Release(l1.ID)
	time.Sleep(100 * time.Millisecond)
	if g, _ := s.Leases.Get(l2.ID, 0); g.State != lease.Queued {
		t.Fatal("broken device must not be handed out")
	}
	st := s.Devices()[0]
	if st.Restarts < 1 {
		t.Fatalf("broken device must be restarted: %+v", st)
	}
	d := s.devs["p1"]
	d.ctl.Lock()
	s.halt(d)
	d.ctl.Unlock()
}

func TestClearFailureRestarts(t *testing.T) {
	s, f := lifecycleService(t, `"release":"clear","release_packages":["com.example.app"],`)
	f.clearErr = errors.New("pm clear failed")
	s.ctx = context.Background()
	l1, _ := s.Leases.Request("u1", "", "", "")
	s.Leases.Release(l1.ID)
	time.Sleep(100 * time.Millisecond)
	if st := s.Devices()[0]; st.Restarts < 1 {
		t.Fatalf("failed cleanup must restart the device: %+v", st)
	}
	if strings.Contains(f.got(), "check") {
		t.Fatal("no health check after a failed cleanup")
	}
	d := s.devs["p1"]
	d.ctl.Lock()
	s.halt(d)
	d.ctl.Unlock()
}

func TestIdleHealthLoop(t *testing.T) {
	s, f := lifecycleService(t, `"health_interval":"10s",`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.healthLoop(ctx, 20*time.Millisecond)
	for i := 0; i < 100 && !strings.Contains(f.got(), "check"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(f.got(), "check") {
		t.Fatal("idle device was never checked")
	}
	if l, _ := s.Leases.Request("u", "", "", ""); l.State != lease.Active {
		// The loop may be mid-check; it releases right after.
		waitState(t, s.Leases, l.ID, lease.Active)
	}
}

func TestPrepareKeepsAwakeAndEnrollsFingerprint(t *testing.T) {
	s, f := lifecycleService(t, "")
	s.prepare(context.Background(), s.devs["p1"], s.Log)
	if got, want := f.got(), "anim,awake,pin"; got != want {
		t.Fatalf("prepare without fingerprint: %s, want %s", got, want)
	}

	cfg, err := config.Parse([]byte(`{"sdk":"/nonexistent","state_dir":"` + t.TempDir() + `",
	  "devices":{"p1":{"unlock_pin":"1234","fingerprint":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s2, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f2 := &fakeOps{}
	s2.opsFn = func(string) deviceOps { return f2 }
	s2.prepare(context.Background(), s2.devs["p1"], s2.Log)
	if got, want := f2.got(), "anim,awake,pin,finger"; got != want {
		t.Fatalf("prepare with fingerprint: %s, want %s", got, want)
	}
}
