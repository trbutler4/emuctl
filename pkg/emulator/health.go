package emulator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// Health is the result of a bounded device check.
type Health int

const (
	// Healthy: adb reached the device and Android reports boot completed.
	Healthy Health = iota
	// Unknown: the check itself failed (timeout, adb error). This is not
	// evidence the device is broken, so callers must not tear it down.
	Unknown
	// Broken: positive evidence the device is unusable, such as its adb port
	// not accepting connections or Android no longer reporting boot completed.
	Broken
)

func (h Health) String() string {
	return [...]string{"healthy", "unknown", "broken"}[h]
}

// Check probes the device within ctx. It only reads state.
func (e *Emulator) Check(ctx context.Context) (Health, error) {
	// The emulator owns its adb port for its whole life. A closed port while
	// the process runs means the emulator is wedged or gone.
	c, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", e.ADBPort()))
	if err != nil {
		if ctx.Err() != nil {
			return Unknown, err
		}
		return Broken, fmt.Errorf("adb port %d closed: %w", e.ADBPort(), err)
	}
	c.Close()
	if err := e.Connect(ctx); err != nil {
		return Unknown, err
	}
	out, err := e.ADB(ctx, "shell", "getprop", "sys.boot_completed")
	return classifyBootProp(string(out), err, ctx.Err())
}

func classifyBootProp(out string, err, ctxErr error) (Health, error) {
	out = strings.TrimSpace(out)
	var ee *exec.ExitError
	switch {
	case ctxErr != nil:
		return Unknown, fmt.Errorf("check timed out: %w", ctxErr)
	case err != nil && !errors.As(err, &ee):
		// adb could not run at all: a host problem, not the device.
		return Unknown, err
	case err != nil:
		// adb ran but failed, e.g. "device offline". Offline can be a
		// transient transport state, so it is not proof on its own.
		return Unknown, fmt.Errorf("adb: %v %s", err, out)
	case out == "1":
		return Healthy, nil
	default:
		// adb answered and Android says it is not booted: it restarted or
		// its system server crashed.
		return Broken, fmt.Errorf("sys.boot_completed=%q", out)
	}
}

// DisableAnimations turns off the three global animation scales. Looping
// animations make UI automation flaky (idle timeouts, stale tap targets).
func (e *Emulator) DisableAnimations(ctx context.Context) error {
	for _, key := range []string{"window_animation_scale", "transition_animation_scale", "animator_duration_scale"} {
		if out, err := e.ADB(ctx, "shell", "settings", "put", "global", key, "0"); err != nil {
			return fmt.Errorf("settings put %s: %v %s", key, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// ClearPackages force-stops and clears each package's data. Packages that are
// not installed are skipped.
func (e *Emulator) ClearPackages(ctx context.Context, pkgs []string) error {
	for _, p := range pkgs {
		out, err := e.ADB(ctx, "shell", "pm", "path", p)
		if err != nil || !strings.Contains(string(out), "package:") {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue // not installed
		}
		e.ADB(ctx, "shell", "am", "force-stop", p)
		if out, err := e.ADB(ctx, "shell", "pm", "clear", p); err != nil || !strings.Contains(string(out), "Success") {
			return fmt.Errorf("pm clear %s: %v %s", p, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
