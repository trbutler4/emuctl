// Package emulator creates AVDs and runs Android emulators.
package emulator

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trbutler4/emuctl/pkg/config"
)

type Emulator struct {
	Cfg  *config.Config
	Name string
	Dev  config.Device
}

func New(c *config.Config, name string) (*Emulator, error) {
	d, ok := c.Devices[name]
	if !ok {
		return nil, fmt.Errorf("unknown device %q (have %v)", name, c.Names())
	}
	if c.SDK == "" {
		return nil, fmt.Errorf("no Android SDK: set \"sdk\" in config or ANDROID_SDK_ROOT")
	}
	return &Emulator{Cfg: c, Name: name, Dev: d}, nil
}

func (e *Emulator) avdName() string { return e.Cfg.AVDName(e.Name) }
func (e *Emulator) avdDir() string  { return filepath.Join(e.Cfg.AVDHome(e.Name), e.avdName()+".avd") }
func (e *Emulator) ADBPort() int    { return e.Dev.ConsolePort + 1 }
func (e *Emulator) Serial() string  { return e.Cfg.Serial(e.Name) }

// Env isolates the emulator's AVD and user state per device.
func (e *Emulator) Env() []string {
	dir := e.Cfg.DeviceDir(e.Name)
	return append(os.Environ(),
		"ANDROID_SDK_ROOT="+e.Cfg.SDK,
		"ANDROID_HOME="+e.Cfg.SDK,
		"ANDROID_USER_HOME="+filepath.Join(dir, "android-user"),
		"ANDROID_AVD_HOME="+e.Cfg.AVDHome(e.Name),
		// The emulator registers with this device's private adb server only,
		// never the user's default 5037 server.
		"ANDROID_ADB_SERVER_PORT="+strconv.Itoa(e.privateServerPort()),
	)
}

func (e *Emulator) Exists() bool {
	_, err := os.Stat(filepath.Join(e.avdDir(), "config.ini"))
	return err == nil
}

// Create makes the AVD if it does not exist yet. It never overwrites one.
func (e *Emulator) Create(ctx context.Context) error {
	if err := e.runGuard(); err != nil {
		return err
	}
	if err := os.MkdirAll(e.Cfg.AVDHome(e.Name), 0o700); err != nil {
		return err
	}
	if !e.Exists() {
		avdmanager := filepath.Join(e.Cfg.SDK, "cmdline-tools", "latest", "bin", "avdmanager")
		if m, _ := filepath.Glob(filepath.Join(e.Cfg.SDK, "cmdline-tools", "*", "bin", "avdmanager")); len(m) > 0 {
			avdmanager = m[0]
		}
		cmd := exec.CommandContext(ctx, avdmanager, "create", "avd", "--name", e.avdName(),
			"--package", e.Dev.Image, "--device", e.Dev.Profile, "--path", e.avdDir())
		cmd.Env = e.Env()
		cmd.Stdin = strings.NewReader("no\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("avdmanager: %w\n%s", err, out)
		}
	}
	return e.applyHardware()
}

// applyHardware rewrites the hardware keys every start so config edits apply
// to existing AVDs.
func (e *Emulator) applyHardware() error {
	path := filepath.Join(e.avdDir(), "config.ini")
	want := map[string]string{
		"hw.ramSize":      strconv.Itoa(e.Dev.RAM),
		"hw.cpu.ncore":    strconv.Itoa(e.Dev.Cores),
		"hw.gpu.enabled":  "yes",
		"hw.lcd.width":    strconv.Itoa(e.Dev.Width),
		"hw.lcd.height":   strconv.Itoa(e.Dev.Height),
		"hw.lcd.density":  strconv.Itoa(e.Dev.Density),
		"hw.keyboard":     "yes",
		"showDeviceFrame": "no",
		"skin.name":       fmt.Sprintf("%dx%d", e.Dev.Width, e.Dev.Height),
		"skin.path":       "_no_skin",
	}
	return rewriteINI(path, want)
}

func rewriteINI(path string, want map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, _, _ := strings.Cut(sc.Text(), "=")
		if _, ok := want[strings.TrimSpace(key)]; !ok {
			lines = append(lines, sc.Text())
		}
	}
	f.Close()
	if err := sc.Err(); err != nil {
		return err
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lines = append(lines, k+"="+want[k])
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Args is the emulator command line.
func (e *Emulator) Args() []string {
	args := []string{
		"-avd", e.avdName(), "-port", strconv.Itoa(e.Dev.ConsolePort),
		"-no-window", "-no-audio", "-no-boot-anim", "-no-snapshot-save",
		"-gpu", e.Dev.GPU, "-accel", "on",
		"-memory", strconv.Itoa(e.Dev.RAM), "-cores", strconv.Itoa(e.Dev.Cores),
		"-skin", fmt.Sprintf("%dx%d", e.Dev.Width, e.Dev.Height),
	}
	return append(args, e.Dev.ExtraArgs...)
}

func (e *Emulator) runGuard() error {
	for _, p := range []int{e.Dev.ConsolePort, e.ADBPort()} {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p), time.Second); err == nil {
			c.Close()
			return fmt.Errorf("port %d already in use: another emulator is running for %s, refusing to start a second one on the same AVD", p, e.Name)
		}
	}
	return nil
}

// Command builds the emulator process, logging to the device log file.
func (e *Emulator) Command(ctx context.Context) (*exec.Cmd, *os.File, error) {
	if err := e.runGuard(); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(e.Cfg.DeviceDir(e.Name), 0o700); err != nil {
		return nil, nil, err
	}
	log, err := os.OpenFile(e.Cfg.LogPath(e.Name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, filepath.Join(e.Cfg.SDK, "emulator", "emulator"), e.Args()...)
	cmd.Env = e.Env()
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 20 * time.Second
	return cmd, log, nil
}

// ADB runs an adb client against this emulator only, without any adb server,
// by connecting the emulator's adb port through a private server.
func (e *Emulator) ADB(ctx context.Context, args ...string) ([]byte, error) {
	adb := filepath.Join(e.Cfg.SDK, "platform-tools", "adb")
	full := append([]string{"-s", fmt.Sprintf("127.0.0.1:%d", e.ADBPort())}, args...)
	cmd := exec.CommandContext(ctx, adb, full...)
	cmd.Env = append(os.Environ(), "ANDROID_ADB_SERVER_PORT="+strconv.Itoa(e.privateServerPort()))
	return cmd.CombinedOutput()
}

// Each device gets its own adb server port so emuctl never disturbs the
// user's default adb server (5037) or anything else on the host.
func (e *Emulator) privateServerPort() int { return 25000 + e.Dev.ConsolePort }

// Connect attaches the private adb server to the emulator's adb port.
func (e *Emulator) Connect(ctx context.Context) error {
	adb := filepath.Join(e.Cfg.SDK, "platform-tools", "adb")
	cmd := exec.CommandContext(ctx, adb, "connect", fmt.Sprintf("127.0.0.1:%d", e.ADBPort()))
	cmd.Env = append(os.Environ(), "ANDROID_ADB_SERVER_PORT="+strconv.Itoa(e.privateServerPort()))
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "failed") || strings.Contains(string(out), "cannot") {
		return fmt.Errorf("adb connect: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Booted reports whether Android has finished booting.
func (e *Emulator) Booted(ctx context.Context) bool {
	if e.Connect(ctx) != nil {
		return false
	}
	out, err := e.ADB(ctx, "shell", "getprop", "sys.boot_completed")
	return err == nil && strings.TrimSpace(string(out)) == "1"
}

// Unlocked reports whether user 0's credential-encrypted storage is unlocked.
// Until it is, Android hides every app that is not direct-boot aware, so
// launcher resolution fails with "No activity found".
func (e *Emulator) Unlocked(ctx context.Context) bool {
	out, err := e.ADB(ctx, "shell", "dumpsys", "user")
	return err == nil && strings.Contains(string(out), "0=RUNNING_UNLOCKED")
}

// Unlock wakes the screen, dismisses the keyguard and enters the device PIN
// when one is configured.
func (e *Emulator) Unlock(ctx context.Context) {
	e.ADB(ctx, "shell", "input", "keyevent", "KEYCODE_WAKEUP")
	e.ADB(ctx, "shell", "wm", "dismiss-keyguard")
	if e.Dev.UnlockPIN != "" {
		time.Sleep(time.Second)
		e.ADB(ctx, "shell", "input", "text", e.Dev.UnlockPIN)
		e.ADB(ctx, "shell", "input", "keyevent", "KEYCODE_ENTER")
	}
}

// WaitBooted polls until boot completes and the user is unlocked, or ctx expires.
func (e *Emulator) WaitBooted(ctx context.Context) error {
	for {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		ok := e.Booted(c)
		if ok && !e.Unlocked(c) {
			e.Unlock(c)
			ok = e.Unlocked(c)
		}
		cancel()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: boot not complete: %w", e.Name, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// KillPrivateServer stops this device's private adb server.
func (e *Emulator) KillPrivateServer(ctx context.Context) {
	adb := filepath.Join(e.Cfg.SDK, "platform-tools", "adb")
	cmd := exec.CommandContext(ctx, adb, "kill-server")
	cmd.Env = append(os.Environ(), "ANDROID_ADB_SERVER_PORT="+strconv.Itoa(e.privateServerPort()))
	_ = cmd.Run()
}
