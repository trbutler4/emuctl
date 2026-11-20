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
	"os"
	"path/filepath"
	"regexp"
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
	// Lease lifetime without a heartbeat, e.g. "2m". Default 2m. Clients may
	// ask for a longer per-lease TTL up to max_lease_ttl (default 4h).
	LeaseTTL    string `json:"lease_ttl,omitempty"`
	MaxLeaseTTL string `json:"max_lease_ttl,omitempty"`
	// Wipe user data and reboot after every lease, so each user starts clean.
	ResetOnRelease bool `json:"reset_on_release,omitempty"`
	// Users and their API tokens. Each value is "sha256:<hex>" of the token.
	Users   map[string]string `json:"users"`
	Devices map[string]Device `json:"devices"`
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func DefaultPath() string {
	if p := os.Getenv("EMUCTL_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "emuctl", "config.json")
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
		c.SDK = os.Getenv("ANDROID_SDK_ROOT")
	}
	if c.SDK == "" {
		c.SDK = os.Getenv("ANDROID_HOME")
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
			d.Image = "system-images;android-36;google_apis;x86_64"
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
	for user, h := range c.Users {
		if !nameRE.MatchString(user) {
			return fmt.Errorf("user %q: name must match %s", user, nameRE)
		}
		if hx, ok := strings.CutPrefix(h, "sha256:"); !ok || len(hx) != 64 {
			return fmt.Errorf("user %s: token must be \"sha256:<64 hex>\" (see `emuctl hash-token`)", user)
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

// Example is written by `emuctl init`.
const Example = `{
  "listen": "0.0.0.0",
  "api": "0.0.0.0:7600",
  "reset_on_release": true,
  "users": {},
  "devices": {
    "pixel-1": { "profile": "pixel_7", "expose_port": 15601 },
    "pixel-2": { "profile": "pixel_7", "expose_port": 15603 }
  }
}
`
