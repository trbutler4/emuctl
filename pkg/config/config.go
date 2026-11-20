// Package config loads the emuctl device inventory.
//
// The config is a JSON file (default ~/.config/emuctl/config.json). Every
// device gets its own AVD, state directory and port pair, so devices never
// share an ADB server or emulator console.
package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// TTL is the parsed lease lifetime.
func (c *Config) TTL() time.Duration { d, _ := time.ParseDuration(c.LeaseTTL); return d }

// MaxTTL caps per-lease TTLs requested by clients.
func (c *Config) MaxTTL() time.Duration { d, _ := time.ParseDuration(c.MaxLeaseTTL); return d }

// HashToken is the stored form of an API token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Authenticate returns the user owning token.
func (c *Config) Authenticate(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	want := HashToken(token)
	for user, h := range c.Users {
		if subtle.ConstantTimeCompare([]byte(h), []byte(want)) == 1 {
			return user, true
		}
	}
	return "", false
}

// Emulator console ports must be even. adb servers scan 5555-5585 for
// emulators at startup, so the default range starts above that to keep
// devices invisible to unrelated adb servers on the same host.
const (
	MinConsolePort     = 5554
	MaxConsolePort     = 5682
	DefaultConsoleBase = 5600
)

type Device struct {
	// Hardware profile passed to avdmanager --device (e.g. pixel_7).
	Profile string `json:"profile,omitempty"`
	// SDK package of the system image.
	Image string `json:"image,omitempty"`
	// Even console port. The emulator's adb port is ConsolePort+1.
	ConsolePort int `json:"console_port,omitempty"`
	// TCP port on Config.Listen that forwards to the emulator adb port.
	// Clients run `adb connect HOST:ExposePort`. 0 disables exposure.
	ExposePort int    `json:"expose_port,omitempty"`
	RAM        int    `json:"ram_mb,omitempty"`
	Cores      int    `json:"cores,omitempty"`
	GPU        string `json:"gpu,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	Density    int    `json:"density,omitempty"`
	// Use an existing AVD instead of creating emuctl-NAME under state_dir.
	AVDHome string `json:"avd_home,omitempty"`
	AVDName string `json:"avd_name,omitempty"`
	// PIN of the AVD's screen lock, if it has one. emuctl enters it after
	// boot because apps are hidden until the user's storage is unlocked.
	UnlockPIN string `json:"unlock_pin,omitempty"`
	// Show the emulator window instead of running headless.
	Window bool `json:"window,omitempty"`
	// Extra emulator flags, appended verbatim.
	ExtraArgs []string `json:"extra_args,omitempty"`
}

type Config struct {
	// Android SDK root. Defaults to $ANDROID_SDK_ROOT or $ANDROID_HOME.
	SDK string `json:"sdk,omitempty"`
	// Per-device state (AVD, logs). Default ~/.local/share/emuctl.
	StateDir string `json:"state_dir,omitempty"`
	// Address the device adb forwarders bind. Default 127.0.0.1.
	Listen string `json:"listen,omitempty"`
	// HTTP API address. Default 127.0.0.1:7600.
	API string `json:"api,omitempty"`
	// Prometheus /metrics address. Empty (default) serves /metrics on the API
	// listener, behind the same token. A separate address serves it without a
	// token, so bind it to loopback or a monitoring network.
	MetricsAddr string `json:"metrics_addr,omitempty"`
	// Lease lifetime without a heartbeat, e.g. "2m". Default 2m. Clients may
	// ask for a longer per-lease TTL up to max_lease_ttl (default 4h).
	LeaseTTL    string `json:"lease_ttl,omitempty"`
	MaxLeaseTTL string `json:"max_lease_ttl,omitempty"`
	// Wipe user data and reboot after every lease. Deprecated: use
	// "release": "wipe".
	ResetOnRelease bool `json:"reset_on_release,omitempty"`
	// What happens to a device when its lease ends, before the next lease:
	//   "none"  nothing (default)
	//   "clear" force-stop and `pm clear` each of release_packages; the
	//           device stays booted and keeps its installed apps
	//   "wipe"  reboot with -wipe-data (slow, removes installed APKs)
	Release string `json:"release,omitempty"`
	// Packages to clear on release with "release": "clear".
	ReleasePackages []string `json:"release_packages,omitempty"`
	// adb shell commands run on every release, after clearing (any mode but
	// wipe). A failing command marks the device broken and restarts it.
	ReleaseCommands []string `json:"release_commands,omitempty"`
	// Turn off window, transition and animator animations after boot and
	// on every release. Default true; animations cause flaky UI automation.
	DisableAnimations *bool `json:"disable_animations,omitempty"`
	// How often idle devices are health-checked. Default 2m, "0" disables.
	HealthInterval string `json:"health_interval,omitempty"`
	// How long a cold boot may take before the emulator is killed and
	// restarted. Default 6m.
	BootTimeout string `json:"boot_timeout,omitempty"`
	// Users and their API tokens. Each value is "sha256:<hex>" of the token.
	// With no users, the API is open to anyone who can reach it, which is
	// only allowed when api and listen are loopback addresses.
	Users   map[string]string `json:"users"`
	Devices map[string]Device `json:"devices"`
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

var pkgRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)

// Release modes.
const (
	ReleaseNone  = "none"
	ReleaseClear = "clear"
	ReleaseWipe  = "wipe"
)

// HealthEvery is the parsed idle health-check interval, 0 when disabled.
func (c *Config) HealthEvery() time.Duration { d, _ := time.ParseDuration(c.HealthInterval); return d }

func DefaultPath() string {
	if p := os.Getenv("EMUCTL_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(ConfigDir(), "config.json")
}

// ConfigDir is $XDG_CONFIG_HOME/emuctl, else ~/.config/emuctl, on every OS.
func ConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "emuctl")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".config", "emuctl")
	}
	d, _ := os.UserConfigDir()
	return filepath.Join(d, "emuctl")
}

// DefaultSDK finds the Android SDK from the environment or the location
// Android Studio installs it to.
func DefaultSDK() string {
	for _, k := range []string{"ANDROID_SDK_ROOT", "ANDROID_HOME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	home, _ := os.UserHomeDir()
	var cands []string
	switch runtime.GOOS {
	case "darwin":
		cands = []string{filepath.Join(home, "Library", "Android", "sdk")}
	case "windows":
		cands = []string{filepath.Join(os.Getenv("LOCALAPPDATA"), "Android", "Sdk")}
	default:
		cands = []string{filepath.Join(home, "Android", "Sdk"), filepath.Join(home, "Android", "sdk")}
	}
	for _, c := range cands {
		if st, err := os.Stat(filepath.Join(c, "emulator")); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}

// DefaultImage picks the newest installed system image for the host CPU,
// preferring google_apis over Play Store images, else android-36 google_apis.
func DefaultImage(sdk string) string {
	abi := "x86_64"
	if runtime.GOARCH == "arm64" {
		abi = "arm64-v8a"
	}
	if imgs := InstalledImages(sdk, abi); len(imgs) > 0 {
		return imgs[0]
	}
	return "system-images;android-36;google_apis;" + abi
}

// InstalledImages lists SDK system images for abi, best first.
func InstalledImages(sdk, abi string) []string {
	if sdk == "" {
		return nil
	}
	m, _ := filepath.Glob(filepath.Join(sdk, "system-images", "*", "*", abi))
	score := func(p string) (float64, int) {
		api, tag := filepath.Base(filepath.Dir(filepath.Dir(p))), filepath.Base(filepath.Dir(p))
		var n float64
		fmt.Sscanf(strings.TrimPrefix(api, "android-"), "%g", &n)
		rank := map[string]int{"google_apis": 3, "default": 2, "google_apis_playstore": 1}[tag]
		return n, rank
	}
	sort.Slice(m, func(i, j int) bool {
		ai, ri := score(m[i])
		aj, rj := score(m[j])
		if ri != rj {
			return ri > rj
		}
		return ai > aj
	})
	out := make([]string, 0, len(m))
	for _, p := range m {
		rel, _ := filepath.Rel(sdk, p)
		out = append(out, strings.ReplaceAll(filepath.ToSlash(rel), "/", ";"))
	}
	return out
}

// Open reports whether the API runs without authentication.
func (c *Config) Open() bool { return len(c.Users) == 0 }

// BootTimeoutDuration is the parsed boot timeout.
func (c *Config) BootTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(c.BootTimeout)
	return d
}

func isLoopback(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no config at %s (run `emuctl init`)", path)
	}
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	return &c, c.Validate()
}

func (c *Config) applyDefaults() {
	if c.SDK == "" {
		c.SDK = DefaultSDK()
	}
	if c.BootTimeout == "" {
		c.BootTimeout = "6m"
	}
	if c.Release == "" {
		c.Release = ReleaseNone
		if c.ResetOnRelease {
			c.Release = ReleaseWipe
		}
	}
	if c.DisableAnimations == nil {
		t := true
		c.DisableAnimations = &t
	}
	if c.HealthInterval == "" {
		c.HealthInterval = "2m"
	}
	if c.StateDir == "" {
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			base = filepath.Join(os.Getenv("HOME"), ".local", "share")
		}
		c.StateDir = filepath.Join(base, "emuctl")
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1"
	}
	if c.API == "" {
		c.API = "127.0.0.1:7600"
	}
	if c.LeaseTTL == "" {
		c.LeaseTTL = "2m"
	}
	if c.MaxLeaseTTL == "" {
		c.MaxLeaseTTL = "4h"
	}
	if c.Devices == nil {
		c.Devices = map[string]Device{}
	}
	for i, name := range c.Names() {
		d := c.Devices[name]
		if d.Profile == "" {
			d.Profile = "pixel_7"
		}
		if d.Image == "" {
			d.Image = DefaultImage(c.SDK)
		}
		if d.ConsolePort == 0 {
			d.ConsolePort = DefaultConsoleBase + 2*i
		}
		if d.RAM == 0 {
			d.RAM = 2048
		}
		if d.Cores == 0 {
			d.Cores = 4
		}
		if d.GPU == "" {
			d.GPU = "swiftshader"
		}
		if d.Width == 0 {
			d.Width = 1080
		}
		if d.Height == 0 {
			d.Height = 2400
		}
		if d.Density == 0 {
			d.Density = 420
		}
		c.Devices[name] = d
	}
}

// Names returns device names in stable order.
func (c *Config) Names() []string {
	names := make([]string, 0, len(c.Devices))
	for n := range c.Devices {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (c *Config) Validate() error {
	ports := map[int]string{}
	claim := func(port int, what string) error {
		if prev, ok := ports[port]; ok {
			return fmt.Errorf("port %d used by both %s and %s", port, prev, what)
		}
		ports[port] = what
		return nil
	}
	if _, err := time.ParseDuration(c.LeaseTTL); err != nil {
		return fmt.Errorf("lease_ttl: %w", err)
	}
	if _, err := time.ParseDuration(c.MaxLeaseTTL); err != nil {
		return fmt.Errorf("max_lease_ttl: %w", err)
	}
	if d, err := time.ParseDuration(c.BootTimeout); err != nil || d <= 0 {
		return fmt.Errorf("boot_timeout: must be a positive duration like \"6m\"")
	}
	if d, _ := time.ParseDuration(c.LeaseTTL); d <= 0 {
		return errors.New("lease_ttl: must be positive")
	}
	if c.MaxTTL() < c.TTL() {
		return errors.New("max_lease_ttl must be at least lease_ttl")
	}
	if d, err := time.ParseDuration(c.HealthInterval); err != nil || d < 0 || (d > 0 && d < 10*time.Second) {
		return errors.New("health_interval: must be \"0\" (off) or at least \"10s\"")
	}
	switch c.Release {
	case ReleaseNone, ReleaseWipe:
	case ReleaseClear:
		if len(c.ReleasePackages) == 0 {
			return errors.New("release \"clear\" needs release_packages, e.g. [\"com.example.app\"]")
		}
	default:
		return fmt.Errorf("release: %q must be none, clear or wipe", c.Release)
	}
	for _, p := range c.ReleasePackages {
		if !pkgRE.MatchString(p) {
			return fmt.Errorf("release_packages: %q is not an Android package name", p)
		}
	}
	if c.Open() && (!isLoopback(c.API) || !isLoopback(c.Listen)) {
		return errors.New("no users configured: api and listen must be loopback (127.0.0.1) unless you add users with `emuctl add-user NAME`")
	}
	for user, h := range c.Users {
		if !nameRE.MatchString(user) {
			return fmt.Errorf("user %q: name must match %s", user, nameRE)
		}
		if hx, ok := strings.CutPrefix(h, "sha256:"); !ok || len(hx) != 64 {
			return fmt.Errorf("user %s: token must be \"sha256:<64 hex>\" (see `emuctl new-token`)", user)
		}
	}
	for _, name := range c.Names() {
		d := c.Devices[name]
		if !nameRE.MatchString(name) {
			return fmt.Errorf("device %q: name must match %s", name, nameRE)
		}
		if d.ConsolePort < MinConsolePort || d.ConsolePort > MaxConsolePort || d.ConsolePort%2 != 0 {
			return fmt.Errorf("device %s: console_port must be even, %d-%d", name, MinConsolePort, MaxConsolePort)
		}
		if err := claim(d.ConsolePort, name+" console"); err != nil {
			return err
		}
		if err := claim(d.ConsolePort+1, name+" adb"); err != nil {
			return err
		}
		if d.ExposePort != 0 {
			if d.ExposePort < 1024 || d.ExposePort > 65535 {
				return fmt.Errorf("device %s: expose_port must be 1024-65535", name)
			}
			if err := claim(d.ExposePort, name+" expose"); err != nil {
				return err
			}
		}
	}
	return nil
}

// Paths for one device.
func (c *Config) DeviceDir(name string) string { return filepath.Join(c.StateDir, "devices", name) }
func (c *Config) AVDHome(name string) string {
	if h := c.Devices[name].AVDHome; h != "" {
		return h
	}
	return filepath.Join(c.DeviceDir(name), "avd")
}
func (c *Config) AVDName(name string) string {
	if n := c.Devices[name].AVDName; n != "" {
		return n
	}
	return "emuctl-" + name
}
func (c *Config) LogPath(name string) string { return filepath.Join(c.DeviceDir(name), "emulator.log") }
func (c *Config) Serial(name string) string {
	return fmt.Sprintf("emulator-%d", c.Devices[name].ConsolePort)
}

// Example is written by `emuctl init`. It runs one local device with no
// authentication. See the README to share devices over a network.
const Example = `{
  "api": "127.0.0.1:7600",
  "listen": "127.0.0.1",
  "users": {},
  "devices": {
    "pixel-1": { "profile": "pixel_7", "expose_port": 15601 }
  }
}
`
