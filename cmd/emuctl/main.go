// Command emuctl manages a pool of Android emulators shared by many users and
// agents: a FIFO lease queue, holder-only adb access, APK install and device
// reset over an HTTP API.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/trbutler4/emuctl/pkg/client"
	"github.com/trbutler4/emuctl/pkg/config"
	"github.com/trbutler4/emuctl/pkg/mcp"
	"github.com/trbutler4/emuctl/pkg/service"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `emuctl: Android emulators for people and agents, with a lease queue and an MCP server

Quick start (one machine):
  emuctl init && emuctl doctor         write ~/.config/emuctl/config.json and check the SDK
  emuctl serve                         boot the emulators and serve the API on 127.0.0.1:7600
  emuctl mcp                           MCP server for agents (add it to your agent's config)

Server (on the emulator host):
  emuctl serve [-config PATH]          run emulators, lease queue and API
  emuctl init [-config PATH]           write a starter config (one local device)
  emuctl doctor [-config PATH]         check the SDK, system images and config
  emuctl add-user NAME [-config PATH]  create a user, print its token once
  emuctl new-token                     print a token (stdout) and its config hash (stderr)

Client (EMUCTL_URL: http://host:7600 or ssh://[user@]host, default http://127.0.0.1:7600;
        token from EMUCTL_TOKEN or ~/.config/emuctl/token):
  emuctl devices                       device states and holders
  emuctl queue                         waiting leases
  emuctl run [flags] -- CMD ARGS...    wait for a device, run CMD, release
      -device NAME   specific device (default: any)
      -apk PATH      install before running (repeatable)
      -apk-key KEY   install a stored APK (skipped if the device has it);
                     exits 3 if the server does not have KEY
      -reset         wipe the device before running
      -note TEXT     shown in queue/status
    CMD gets ANDROID_SERIAL / EMUCTL_SERIAL / EMUCTL_DEVICE / EMUCTL_LEASE.
    Maestro: emuctl run -apk app.apk -- maestro test flows/
  emuctl acquire [-device NAME] [-note TEXT]   print lease JSON, keep it alive
  emuctl release LEASE
  emuctl install LEASE APK
  emuctl install LEASE -key KEY        install a stored APK
  emuctl apk list                      APKs stored on the server
  emuctl apk has KEY                   exit 0 if stored, 3 if not
  emuctl apk put KEY FILE              store an APK (exists = success)
  emuctl reset LEASE [-wipe]
  emuctl shell LEASE CMD...            adb shell on the device, run by the server
  emuctl screenshot LEASE FILE.png
  emuctl logs DEVICE
  emuctl mcp                           MCP server on stdio for agents
  emuctl version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	code := 0
	switch cmd {
	case "serve":
		err = serve(ctx, args)
	case "init":
		err = initConfig(args)
	case "add-user":
		err = addUser(args)
	case "new-token":
		b := make([]byte, 24)
		rand.Read(b)
		token := "emu_" + hex.EncodeToString(b)
		fmt.Println(token)
		fmt.Fprintln(os.Stderr, "config users entry:", config.HashToken(token))
	case "devices":
		err = devices(ctx)
	case "queue":
		err = queue(ctx)
	case "run":
		code, err = run(ctx, args)
	case "acquire":
		err = acquire(ctx, args)
	case "release":
		err = need(args, 1, func() error { return client.FromEnv().Release(ctx, args[0]) })
	case "install":
		err = need(args, 2, func() error {
			if args[1] == "-key" {
				if len(args) < 3 {
					return errors.New("usage: emuctl install LEASE -key KEY")
				}
				installed, err := client.FromEnv().InstallStored(ctx, args[0], args[2])
				if errors.Is(err, client.ErrAPKNotStored) {
					code = exitNotStored
				}
				if err == nil && !installed {
					fmt.Fprintln(os.Stderr, "emuctl: already installed")
				}
				return err
			}
			f, err := os.Open(args[1])
			if err != nil {
				return err
			}
			defer f.Close()
			out, err := client.FromEnv().Install(ctx, args[0], f)
			if err == nil {
				fmt.Println(out["output"])
			}
			return err
		})
	case "reset":
		err = need(args, 1, func() error {
			wipe := len(args) > 1 && args[1] == "-wipe"
			c := client.FromEnv()
			if _, err := c.Reset(ctx, args[0], wipe); err != nil {
				return err
			}
			_, err := c.WaitReady(ctx, args[0])
			return err
		})
	case "logs":
		err = need(args, 1, func() error {
			s, err := client.FromEnv().Logs(ctx, args[0])
			fmt.Print(s)
			return err
		})
	case "apk":
		code, err = apkCmd(ctx, args)
	case "doctor":
		err = doctor(args)
	case "shell":
		err = need(args, 2, func() error {
			res, err := client.FromEnv().Shell(ctx, args[0], strings.Join(args[1:], " "))
			if err != nil {
				return err
			}
			fmt.Print(res.Output)
			code = res.ExitCode
			return nil
		})
	case "screenshot":
		err = need(args, 2, func() error {
			png, err := client.FromEnv().Screenshot(ctx, args[0])
			if err != nil {
				return err
			}
			return os.WriteFile(args[1], png, 0o644)
		})
	case "version", "-version", "--version":
		fmt.Println("emuctl", version)
	case "mcp":
		c := client.FromEnv()
		err = mcp.New(c, version).Serve(ctx, os.Stdin, os.Stdout)
		c.Close()
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "emuctl:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func need(args []string, n int, f func() error) error {
	if len(args) < n {
		return errors.New("missing arguments, see `emuctl help`")
	}
	return f()
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", config.DefaultPath(), "config file")
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := configFlag(fs)
	fs.Parse(args)
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	if c.Open() {
		fmt.Fprintln(os.Stderr, "emuctl: no users configured, API is unauthenticated on", c.API)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	s, err := service.New(c, log)
	if err != nil {
		return err
	}
	return s.Run(ctx)
}

func initConfig(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := configFlag(fs)
	fs.Parse(args)
	if _, err := os.Stat(*path); err == nil {
		return fmt.Errorf("%s exists", *path)
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(*path, []byte(config.Example), 0o600); err != nil {
		return err
	}
	fmt.Println("wrote", *path)
	return nil
}

func addUser(args []string) error {
	fs := flag.NewFlagSet("add-user", flag.ExitOnError)
	path := configFlag(fs)
	if len(args) < 1 {
		return errors.New("usage: emuctl add-user NAME")
	}
	name := args[0]
	fs.Parse(args[1:])
	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	users, _ := doc["users"].(map[string]any)
	if users == nil {
		users = map[string]any{}
	}
	if _, ok := users[name]; ok {
		return fmt.Errorf("user %s exists", name)
	}
	b := make([]byte, 24)
	rand.Read(b)
	token := "emu_" + hex.EncodeToString(b)
	users[name] = config.HashToken(token)
	doc["users"] = users
	out, _ := json.MarshalIndent(doc, "", "  ")
	if _, err := config.Parse(out); err != nil {
		return err
	}
	if err := os.WriteFile(*path, append(out, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "added %s (restart emuctl serve to apply). Token, shown once:\n", name)
	fmt.Println(token)
	return nil
}

func devices(ctx context.Context) error {
	ds, err := client.FromEnv().Devices(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "DEVICE\tSTATE\tADB\tHOLDER\tRESTARTS")
	for _, d := range ds {
		holder := "-"
		if d.Holder != nil {
			holder = fmt.Sprintf("%s (%s)", d.Holder.User, d.Holder.Note)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", d.Name, d.State, d.ADB, holder, d.Restarts)
	}
	return w.Flush()
}

func queue(ctx context.Context) error {
	q, err := client.FromEnv().Queue(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "POS\tUSER\tWANT\tNOTE")
	for _, l := range q {
		want := l.Want
		if want == "" {
			want = "any"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", l.Position, l.User, want, l.Note)
	}
	return w.Flush()
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func progress(l client.Lease) {
	switch l.State {
	case "queued":
		fmt.Fprintf(os.Stderr, "emuctl: queued at position %d\n", l.Position)
	case "active":
		if !l.Ready {
			fmt.Fprintf(os.Stderr, "emuctl: got %s, waiting for boot\n", l.Device)
		}
	}
}

// adbConnect attaches the local adb server to the leased device so tools that
// only talk to the adb server (Maestro, Android Studio) can see it.
func adbConnect(ctx context.Context, target string) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err := exec.CommandContext(ctx, "adb", "connect", target).CombinedOutput()
		s := string(out)
		if err == nil && strings.Contains(s, "connected to") {
			wait := exec.CommandContext(ctx, "adb", "-s", target, "wait-for-device")
			if err := wait.Run(); err == nil {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("adb connect %s: %v %s", target, err, strings.TrimSpace(s))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func adbDisconnect(target string) {
	exec.Command("adb", "disconnect", target).Run()
}

func run(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	device := fs.String("device", "", "device name (default any)")
	note := fs.String("note", "", "note shown to other users")
	reset := fs.Bool("reset", false, "wipe the device first")
	var apks multi
	fs.Var(&apks, "apk", "APK to install first (repeatable)")
	apkKey := fs.String("apk-key", "", "stored APK to install first (skipped if the device has it)")
	fs.Parse(args)
	argv := fs.Args()
	if len(argv) == 0 {
		return 2, errors.New("usage: emuctl run [flags] -- CMD ARGS...")
	}
	if *note == "" {
		*note = strings.Join(argv, " ")
		if len(*note) > 80 {
			*note = (*note)[:80]
		}
	}
	c := client.FromEnv()
	defer c.Close()
	// Check the store before queueing, so a miss never holds a device.
	if *apkKey != "" {
		if ok, err := hasAPK(ctx, c, *apkKey); err != nil {
			return 1, err
		} else if !ok {
			return exitNotStored, fmt.Errorf("apk %s is not stored on the server (build it, then `emuctl apk put %s FILE`)", *apkKey, *apkKey)
		}
	}
	l, err := c.Acquire(ctx, *device, *note, progress)
	if err != nil {
		return 1, err
	}
	defer c.Release(context.Background(), l.ID)
	fmt.Fprintf(os.Stderr, "emuctl: leased %s (%s)\n", l.Device, l.ID)

	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	lost := make(chan error, 1)
	go func() { lost <- c.Heartbeat(hbCtx, l.ID, 20*time.Second) }()

	if *reset {
		if _, err := c.Reset(ctx, l.ID, true); err != nil {
			return 1, err
		}
		fmt.Fprintln(os.Stderr, "emuctl: wiping device")
		if l, err = c.WaitReady(ctx, l.ID); err != nil {
			return 1, err
		}
	}
	if *apkKey != "" {
		installed, err := c.InstallStored(ctx, l.ID, *apkKey)
		if errors.Is(err, client.ErrAPKNotStored) {
			return exitNotStored, err
		}
		if err != nil {
			return 1, err
		}
		if installed {
			fmt.Fprintf(os.Stderr, "emuctl: installed %s\n", *apkKey)
		} else {
			fmt.Fprintf(os.Stderr, "emuctl: %s already installed\n", *apkKey)
		}
	}
	for _, apk := range apks {
		f, err := os.Open(apk)
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stderr, "emuctl: installing %s\n", apk)
		_, err = c.Install(ctx, l.ID, f)
		f.Close()
		if err != nil {
			return 1, err
		}
	}
	if l.ADB == "" {
		return 1, fmt.Errorf("device %s has no expose_port, adb is not reachable remotely", l.Device)
	}
	if l, err = c.LocalADB(ctx, l); err != nil {
		return 1, err
	}
	if err := adbConnect(ctx, l.ADB); err != nil {
		return 1, err
	}
	defer adbDisconnect(l.ADB)

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(),
		"ANDROID_SERIAL="+l.ADB, "EMUCTL_SERIAL="+l.ADB,
		"EMUCTL_DEVICE="+l.Device, "EMUCTL_LEASE="+l.ID)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		return 1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case herr := <-lost:
		if herr == nil { // interrupted, ctx cancels the command
			err = <-done
			break
		}
		cmd.Process.Signal(os.Interrupt)
		<-done
		return 1, fmt.Errorf("lease lost during run: %w", herr)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// exitNotStored tells scripts the requested APK key is missing on the server,
// so they can build, `emuctl apk put`, and retry.
const exitNotStored = 3

func hasAPK(ctx context.Context, c *client.Client, key string) (bool, error) {
	list, err := c.APKs(ctx)
	if err != nil {
		return false, err
	}
	for _, a := range list {
		if a.Key == key {
			return true, nil
		}
	}
	return false, nil
}

func apkCmd(ctx context.Context, args []string) (int, error) {
	c := client.FromEnv()
	defer c.Close()
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch {
	case sub == "list":
		list, err := c.APKs(ctx)
		if err != nil {
			return 1, err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "KEY\tMB\tLAST USED")
		for _, a := range list {
			fmt.Fprintf(w, "%s\t%.1f\t%s\n", a.Key, float64(a.Bytes)/(1<<20), a.LastUsed.Local().Format(time.DateTime))
		}
		return 0, w.Flush()
	case sub == "has" && len(args) == 2:
		ok, err := hasAPK(ctx, c, args[1])
		if err != nil {
			return 1, err
		}
		if !ok {
			return exitNotStored, nil
		}
		return 0, nil
	case sub == "put" && len(args) == 3:
		f, err := os.Open(args[2])
		if err != nil {
			return 1, err
		}
		defer f.Close()
		err = c.PutAPK(ctx, args[1], f)
		if errors.Is(err, client.ErrAPKExists) {
			fmt.Fprintf(os.Stderr, "emuctl: %s already stored\n", args[1])
			return 0, nil
		}
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stderr, "emuctl: stored %s\n", args[1])
		return 0, nil
	}
	return 2, errors.New("usage: emuctl apk list | has KEY | put KEY FILE")
}

func acquire(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("acquire", flag.ExitOnError)
	device := fs.String("device", "", "device name (default any)")
	note := fs.String("note", "", "note shown to other users")
	fs.Parse(args)
	c := client.FromEnv()
	defer c.Close()
	l, err := c.Acquire(ctx, *device, *note, progress)
	if err != nil {
		return err
	}
	if l, err = c.LocalADB(ctx, l); err != nil {
		c.Release(context.Background(), l.ID)
		return err
	}
	json.NewEncoder(os.Stdout).Encode(l)
	fmt.Fprintf(os.Stderr, "emuctl: holding %s, Ctrl-C to release. adb connect %s\n", l.Device, l.ADB)
	err = c.Heartbeat(ctx, l.ID, 20*time.Second)
	c.Release(context.Background(), l.ID)
	return err
}

// doctor checks that `emuctl serve` can start every configured device.
func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	path := configFlag(fs)
	fs.Parse(args)
	bad := 0
	check := func(ok bool, what, fix string) {
		if ok {
			fmt.Println("ok  ", what)
			return
		}
		bad++
		fmt.Println("FAIL", what)
		if fix != "" {
			fmt.Println("     ", fix)
		}
	}
	c, err := config.Load(*path)
	check(err == nil, "config "+*path, fmt.Sprint(err))
	if err != nil {
		return errors.New("config problem")
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	check(c.SDK != "", "Android SDK found", "install Android Studio or the command-line tools, then set \"sdk\" in the config or ANDROID_SDK_ROOT")
	if c.SDK != "" {
		check(exists(filepath.Join(c.SDK, "emulator", "emulator")), "emulator in "+c.SDK, "sdkmanager emulator")
		check(exists(filepath.Join(c.SDK, "platform-tools", "adb")), "adb in "+c.SDK, "sdkmanager platform-tools")
		for _, name := range c.Names() {
			d := c.Devices[name]
			avd := filepath.Join(c.AVDHome(name), c.AVDName(name)+".avd", "config.ini")
			if exists(avd) {
				check(true, name+": AVD "+filepath.Dir(avd), "")
				continue
			}
			img := filepath.Join(append([]string{c.SDK}, strings.Split(d.Image, ";")...)...)
			check(exists(img), name+": system image "+d.Image+" (AVD created on first serve)",
				fmt.Sprintf("install it with sdkmanager %q or Android Studio > Settings > SDK Platforms, or set \"image\" to one you have", d.Image))
		}
	}
	if kvm := "/dev/kvm"; runtime.GOOS == "linux" {
		f, err := os.OpenFile(kvm, os.O_RDWR, 0)
		if err == nil {
			f.Close()
		}
		check(err == nil, "hardware acceleration ("+kvm+")", "enable virtualization and add yourself to the kvm group")
	}
	if c.Open() {
		fmt.Println("note  no users: API is unauthenticated, loopback only (" + c.API + ")")
	} else {
		fmt.Printf("note  %d user(s), API on %s, adb forwarders on %s\n", len(c.Users), c.API, c.Listen)
	}
	if bad > 0 {
		return fmt.Errorf("%d problem(s)", bad)
	}
	return nil
}
