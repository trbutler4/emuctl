package emulator

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trbutler4/emuctl/pkg/config"
)

// fakeConsole speaks the emulator console protocol: banner, token auth, OK/KO.
type fakeConsole struct {
	mu   sync.Mutex
	cmds []string
	ln   net.Listener
}

func startConsole(t *testing.T, token string, handle func(cmd string) (string, bool)) (*fakeConsole, int, string) {
	t.Helper()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, ".emulator_console_auth_token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeConsole{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				w := bufio.NewWriter(c)
				w.WriteString("Android Console: Authentication required\r\n" +
					"Android Console: type 'auth <auth_token>' to authenticate\r\n" +
					"Android Console: you can find your <auth_token> in \r\n'" + tokenFile + "'\r\nOK\r\n")
				w.Flush()
				authed := false
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.TrimSpace(line)
					f.mu.Lock()
					f.cmds = append(f.cmds, cmd)
					f.mu.Unlock()
					switch {
					case cmd == "quit":
						return
					case strings.HasPrefix(cmd, "auth "):
						if strings.TrimPrefix(cmd, "auth ") != token {
							w.WriteString("KO: authentication token does not match\r\n")
						} else {
							authed = true
							w.WriteString("Android Console: type 'help' for a list of commands\r\nOK\r\n")
						}
					case !authed:
						w.WriteString("KO: unknown command, try 'help'\r\n")
					default:
						out, ok := handle(cmd)
						if out != "" {
							w.WriteString(out + "\r\n")
						}
						if ok {
							w.WriteString("OK\r\n")
						} else {
							w.WriteString("KO: bad command\r\n")
						}
					}
					w.Flush()
				}
			}(conn)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	return f, port, dir
}

func (f *fakeConsole) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func testEmu(t *testing.T, port int, state string) *Emulator {
	return &Emulator{Cfg: &config.Config{StateDir: state}, Name: "p1", Dev: config.Device{ConsolePort: port}}
}

func TestConsoleAuthAndFingerTouch(t *testing.T) {
	f, port, _ := startConsole(t, "s3cret", func(cmd string) (string, bool) {
		return "", strings.HasPrefix(cmd, "finger ")
	})
	e := testEmu(t, port, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.FingerTouch(ctx, 1); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.got(), ",")
	if !strings.Contains(got, "auth s3cret,finger touch 1,finger remove 1") {
		t.Fatalf("console commands: %s", got)
	}
}

func TestConsoleReportsKO(t *testing.T) {
	_, port, _ := startConsole(t, "tok", func(cmd string) (string, bool) { return "", false })
	e := testEmu(t, port, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.Console(ctx, "nope"); err == nil || !strings.Contains(err.Error(), "bad command") {
		t.Fatalf("want KO error, got %v", err)
	}
}

func TestHostScreenshotWaitsForCompletePNG(t *testing.T) {
	png := "\x89PNG\r\n\x1a\nxxxxxxxxxxxxxxxxIEND\xaeB`\x82"
	_, port, _ := startConsole(t, "tok", func(cmd string) (string, bool) {
		dir, ok := strings.CutPrefix(cmd, "screenrecord screenshot ")
		if !ok {
			return "", false
		}
		// Like the emulator: answer first, finish writing the file after.
		go func() {
			p := filepath.Join(dir, "Screenshot_1.png")
			os.WriteFile(p, []byte(png[:10]), 0o600)
			time.Sleep(200 * time.Millisecond)
			os.WriteFile(p, []byte(png), 0o600)
		}()
		return "", true
	})
	state := t.TempDir()
	e := testEmu(t, port, state)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := e.HostScreenshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != png {
		t.Fatalf("got %q", b)
	}
	left, _ := filepath.Glob(filepath.Join(state, "devices", "p1", "shot-*"))
	if len(left) != 0 {
		t.Fatalf("temp dir not removed: %v", left)
	}
}

func TestTokenPath(t *testing.T) {
	banner := "Android Console: Authentication required\nAndroid Console: you can find your <auth_token> in \n'/var/lib/emuctl/.emulator_console_auth_token'\n"
	if got := tokenPath(banner); got != "/var/lib/emuctl/.emulator_console_auth_token" {
		t.Fatal(got)
	}
	if got := tokenPath("no path here\n"); got != "" {
		t.Fatal(got)
	}
}

// Real uiautomator dumps (trimmed) from an API 36 emulator.
const (
	pinPromptXML    = `<hierarchy rotation="0"><node index="0" text="" resource-id="com.android.systemui:id/layout" package="com.android.systemui" bounds="[0,0][1080,2400]"><node text="Unlock Oku" resource-id="com.android.systemui:id/title" package="com.android.systemui" bounds="[144,470][936,560]" /><node text="" resource-id="com.android.systemui:id/lockPassword" content-desc="PIN area" package="com.android.systemui" bounds="[240,1154][840,1314]" /></node></hierarchy>`
	fingerPromptXML = `<hierarchy rotation="0"><node text="" resource-id="com.android.systemui:id/biometric_prompt_constraint_layout" package="com.android.systemui" bounds="[0,0][1080,2400]"><node text="Verify it’s you" resource-id="com.android.systemui:id/title" package="com.android.systemui" bounds="[100,1500][980,1600]" /><node text="" resource-id="com.android.systemui:id/biometric_icon" package="com.android.systemui" bounds="[440,1800][640,2000]" /><node text="Use PIN" resource-id="com.android.systemui:id/button_use_credential" package="com.android.systemui" bounds="[72,2160][336,2304]" /></node></hierarchy>`
	appXML          = `<hierarchy rotation="0"><node text="Total balance" resource-id="" package="trade.oku.wallet" bounds="[0,200][1080,300]" /></hierarchy>`
)

func TestClassifyPrompts(t *testing.T) {
	cases := []struct {
		name, focus, xml, want string
	}{
		{"pin entry", "BiometricPrompt", pinPromptXML, PromptCredential},
		{"fingerprint prompt", "BiometricPrompt", fingerPromptXML, PromptBiometric},
		{"focus only (ui dump failed)", "BiometricPrompt", "", PromptBiometric},
		{"keyguard", "NotificationShade", "", PromptKeyguard},
		{"app", "trade.oku.wallet/trade.oku.wallet.MainActivity", appXML, PromptNone},
	}
	for _, c := range cases {
		if got := classify(c.focus, parseUI(c.xml)); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestParseUIAndFind(t *testing.T) {
	ui := parseUI(fingerPromptXML)
	n, ok := find(ui, useCredentialRE)
	if !ok || n.X != 204 || n.Y != 2232 {
		t.Fatalf("use PIN button: %+v %v", n, ok)
	}
	n, ok = find(parseUI(pinPromptXML), credentialRE)
	if !ok || n.X != 540 || n.Y != 1234 {
		t.Fatalf("PIN field: %+v %v", n, ok)
	}
}

func TestFocusRE(t *testing.T) {
	for line, want := range map[string]string{
		"  mCurrentFocus=Window{a223514 u0 BiometricPrompt}":                                      "BiometricPrompt",
		"  mCurrentFocus=Window{9935111 u0 trade.oku.wallet/trade.oku.wallet.MainActivity}":       "trade.oku.wallet/trade.oku.wallet.MainActivity",
		"  mCurrentFocus=Window{1b550d7 u0 trade.oku.wallet/expo.modules.devlauncher.launcher.X}": "trade.oku.wallet/expo.modules.devlauncher.launcher.X",
	} {
		m := focusRE.FindStringSubmatch(line)
		if m == nil || m[1] != want {
			t.Errorf("%q: got %v", line, m)
		}
	}
	if m := enrolledR.FindStringSubmatch(`{"service":"FingerprintProvider/default","prints":[{"id":0,"count":1,"accept":3}]}`); m == nil || m[1] != "1" {
		t.Fatal(m)
	}
}
