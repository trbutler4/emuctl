package emulator

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Apps that guard themselves with the device lock (expo-local-authentication,
// androidx.biometric) show a system BiometricPrompt on launch. It is a secure
// window: `adb screencap` returns it as a black frame, and an agent that only
// looks at screenshots sees a black screen. These helpers find and pass it.

// Prompt kinds returned by Prompt.
const (
	PromptNone       = "none"       // nothing is asking for credentials
	PromptBiometric  = "biometric"  // BiometricPrompt waiting for a fingerprint
	PromptCredential = "credential" // a PIN/password entry (BiometricPrompt, Settings or the bouncer)
	PromptKeyguard   = "keyguard"   // the lock screen
)

// FingerID is the finger emuctl enrolls and touches.
const FingerID = 1

var (
	focusRE   = regexp.MustCompile(`mCurrentFocus=Window\{\S+ \S+ ([^}]*)\}`)
	nodeRE    = regexp.MustCompile(`<node [^>]*>`)
	attrRE    = regexp.MustCompile(`([a-z-]+)="([^"]*)"`)
	boundsRE  = regexp.MustCompile(`\[(\d+),(\d+)\]\[(\d+),(\d+)\]`)
	enrolledR = regexp.MustCompile(`"count":(\d+)`)
)

// uiNode is one node of a uiautomator dump.
type uiNode struct {
	Text, Desc, ID, Pkg string
	X, Y                int // centre
}

func parseUI(xml string) []uiNode {
	var out []uiNode
	for _, n := range nodeRE.FindAllString(xml, -1) {
		a := map[string]string{}
		for _, m := range attrRE.FindAllStringSubmatch(n, -1) {
			a[m[1]] = m[2]
		}
		u := uiNode{Text: a["text"], Desc: a["content-desc"], ID: a["resource-id"], Pkg: a["package"]}
		if b := boundsRE.FindStringSubmatch(a["bounds"]); b != nil {
			x1, _ := strconv.Atoi(b[1])
			y1, _ := strconv.Atoi(b[2])
			x2, _ := strconv.Atoi(b[3])
			y2, _ := strconv.Atoi(b[4])
			u.X, u.Y = (x1+x2)/2, (y1+y2)/2
		}
		out = append(out, u)
	}
	return out
}

// Focus is the window that has input focus, e.g. "BiometricPrompt" or
// "trade.oku.wallet/trade.oku.wallet.MainActivity".
func (e *Emulator) Focus(ctx context.Context) string {
	out, _ := e.ADB(ctx, "shell", "dumpsys window | grep -m1 mCurrentFocus")
	if m := focusRE.FindStringSubmatch(string(out)); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// UI returns the current view hierarchy. uiautomator fails while another
// UiAutomation client (Maestro) is attached or the screen keeps changing, so
// it retries briefly.
func (e *Emulator) UI(ctx context.Context) ([]uiNode, error) {
	var last error
	for i := 0; i < 3; i++ {
		out, err := e.ADB(ctx, "shell",
			"uiautomator dump /data/local/tmp/emuctl-ui.xml >/dev/null 2>&1 && cat /data/local/tmp/emuctl-ui.xml")
		if err == nil && strings.Contains(string(out), "<hierarchy") {
			return parseUI(string(out)), nil
		}
		last = fmt.Errorf("uiautomator dump: %v %.200s", err, out)
		if !sleep(ctx, time.Second) {
			return nil, ctx.Err()
		}
	}
	return nil, last
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// classify decides what is asking for credentials from the focused window and,
// when it is not obvious from the focus alone, the view hierarchy.
func classify(focus string, ui []uiNode) string {
	has := func(id string) bool {
		for _, n := range ui {
			if strings.HasSuffix(n.ID, ":id/"+id) {
				return true
			}
		}
		return false
	}
	switch {
	case has("lockPassword") || has("auth_credential_input") || has("password_entry") ||
		has("pinEntry") || has("passwordEntry") || has("lockPattern"):
		return PromptCredential
	case has("biometric_icon") || has("button_use_credential") || focus == "BiometricPrompt":
		return PromptBiometric
	case strings.HasPrefix(focus, "NotificationShade") || has("keyguard_root_view") || has("keyguard_status_view"):
		return PromptKeyguard
	}
	return PromptNone
}

// Prompt reports what, if anything, is asking for the device credential.
func (e *Emulator) Prompt(ctx context.Context) (kind, focus string, ui []uiNode) {
	focus = e.Focus(ctx)
	ui, _ = e.UI(ctx)
	return classify(focus, ui), focus, ui
}

// Fingerprints is the number of enrolled fingerprints, -1 when unknown.
func (e *Emulator) Fingerprints(ctx context.Context) int {
	out, err := e.ADB(ctx, "shell", "dumpsys fingerprint")
	if err != nil {
		return -1
	}
	m := enrolledR.FindStringSubmatch(string(out))
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// HasPIN reports whether the device has a screen lock credential.
func (e *Emulator) HasPIN(ctx context.Context) (bool, error) {
	out, err := e.ADB(ctx, "shell", "locksettings verify")
	s := string(out)
	switch {
	case strings.Contains(s, "has a lock credential") || strings.Contains(s, "old credential was not provided"):
		return true, nil
	case strings.Contains(s, "verified successfully"):
		return false, nil
	}
	return false, fmt.Errorf("locksettings verify: %v %s", err, strings.TrimSpace(s))
}

func (e *Emulator) tap(ctx context.Context, n uiNode) {
	e.ADB(ctx, "shell", "input", "tap", strconv.Itoa(n.X), strconv.Itoa(n.Y))
}

func find(ui []uiNode, re *regexp.Regexp) (uiNode, bool) {
	for _, n := range ui {
		if re.MatchString(n.Text) || re.MatchString(n.Desc) || re.MatchString(n.ID) {
			return n, true
		}
	}
	return uiNode{}, false
}

var (
	useCredentialRE = regexp.MustCompile(`(?i)^(use (pin|password|pattern)|:id/button_use_credential)$|:id/button_use_credential$`)
	credentialRE    = regexp.MustCompile(`:id/(lockPassword|auth_credential_input|password_entry|pinEntry|passwordEntry)$`)
)

func (e *Emulator) enterPIN(ctx context.Context, ui []uiNode) error {
	if e.Dev.UnlockPIN == "" {
		return errors.New("the device asks for its PIN but no unlock_pin is configured for it")
	}
	if n, ok := find(ui, credentialRE); ok {
		e.tap(ctx, n)
	}
	e.ADB(ctx, "shell", "input", "text", e.Dev.UnlockPIN)
	_, err := e.ADB(ctx, "shell", "input", "keyevent", "KEYCODE_ENTER")
	return err
}

// UnlockResult says what UnlockScreen found and did.
type UnlockResult struct {
	Found   []string `json:"found"`           // prompts seen, in order
	Actions []string `json:"actions"`         // what emuctl did about them
	Focus   string   `json:"focus"`           // focused window afterwards
	Cleared bool     `json:"cleared"`         // nothing asks for credentials any more
	Error   string   `json:"error,omitempty"` // why it is still locked
}

// UnlockScreen gets past whatever is asking for the device credential: the
// lock screen, an app's BiometricPrompt (with the enrolled fingerprint) or a
// PIN entry (with unlock_pin). It never types the PIN unless a PIN field is
// showing, so it is safe to call at any time.
func (e *Emulator) UnlockScreen(ctx context.Context) UnlockResult {
	var r UnlockResult
	fingerTries := 0
	for step := 0; step < 8; step++ {
		kind, focus, ui := e.Prompt(ctx)
		r.Focus = focus
		if kind == PromptNone {
			r.Cleared = true
			return r
		}
		r.Found = append(r.Found, kind)
		switch kind {
		case PromptKeyguard:
			e.ADB(ctx, "shell", "input", "keyevent", "KEYCODE_WAKEUP")
			e.ADB(ctx, "shell", "wm", "dismiss-keyguard")
			r.Actions = append(r.Actions, "dismiss-keyguard")
		case PromptBiometric:
			if fingerTries < 2 && e.Fingerprints(ctx) > 0 {
				fingerTries++
				// The sensor only listens once the prompt has settled.
				sleep(ctx, 700*time.Millisecond)
				if err := e.FingerTouch(ctx, FingerID); err != nil {
					r.Actions = append(r.Actions, "finger-touch failed: "+err.Error())
				} else {
					r.Actions = append(r.Actions, "finger-touch")
				}
			} else if n, ok := find(ui, useCredentialRE); ok {
				e.tap(ctx, n)
				r.Actions = append(r.Actions, "use-pin")
			} else {
				r.Error = "a fingerprint prompt is showing, no fingerprint is enrolled and there is no PIN button"
				return r
			}
		case PromptCredential:
			if err := e.enterPIN(ctx, ui); err != nil {
				r.Error = err.Error()
				return r
			}
			r.Actions = append(r.Actions, "pin")
		}
		if !sleep(ctx, 1500*time.Millisecond) {
			r.Error = ctx.Err().Error()
			return r
		}
	}
	kind, focus, _ := e.Prompt(ctx)
	r.Focus, r.Cleared = focus, kind == PromptNone
	if !r.Cleared {
		r.Error = "still showing " + kind + " after several attempts"
	}
	return r
}

// KeepAwake keeps the screen on while the emulator is "plugged in" (always),
// so the lock screen never comes back in the middle of a lease.
func (e *Emulator) KeepAwake(ctx context.Context) error {
	if out, err := e.ADB(ctx, "shell", "settings", "put", "global", "stay_on_while_plugged_in", "7"); err != nil {
		return fmt.Errorf("stay awake: %v %s", err, strings.TrimSpace(string(out)))
	}
	e.ADB(ctx, "shell", "input", "keyevent", "KEYCODE_WAKEUP")
	return nil
}

var enrollStepRE = regexp.MustCompile(`(?i)^(more|i agree|agree|start|next|continue)$`)

// EnsureFingerprint sets the device PIN when the device has no screen lock and
// enrolls finger FingerID, so apps' fingerprint prompts can be passed with
// FingerTouch. It does nothing when a fingerprint is already enrolled.
func (e *Emulator) EnsureFingerprint(ctx context.Context) error {
	if e.Dev.UnlockPIN == "" {
		return errors.New("fingerprint enrollment needs unlock_pin")
	}
	if e.Fingerprints(ctx) > 0 {
		return nil
	}
	has, err := e.HasPIN(ctx)
	if err != nil {
		return err
	}
	if !has {
		if out, err := e.ADB(ctx, "shell", "locksettings", "set-pin", e.Dev.UnlockPIN); err != nil {
			return fmt.Errorf("locksettings set-pin: %v %s", err, strings.TrimSpace(string(out)))
		}
	}
	defer func() {
		bg := context.WithoutCancel(ctx)
		e.ADB(bg, "shell", "am", "force-stop", "com.android.settings")
		e.ADB(bg, "shell", "input", "keyevent", "KEYCODE_HOME")
	}()
	e.ADB(ctx, "shell", "input", "keyevent", "KEYCODE_WAKEUP")
	e.ADB(ctx, "shell", "wm", "dismiss-keyguard")
	// A Settings task left open would be brought to the front instead of the
	// wizard starting, so start from a fresh Settings process and task.
	e.ADB(ctx, "shell", "am", "force-stop", "com.android.settings")
	if out, err := e.ADB(ctx, "shell", "am", "start", "-W", "--activity-clear-task", "-f", "0x10000000",
		"-a", "android.settings.FINGERPRINT_ENROLL"); err != nil {
		return fmt.Errorf("start enrollment: %v %s", err, strings.TrimSpace(string(out)))
	}
	sleep(ctx, 2*time.Second)
	// Walk the Settings wizard: confirm the PIN, accept the intro screens,
	// then touch the sensor until the fingerprint is added.
	last := ""
	for step := 0; step < 40; step++ {
		if e.Fingerprints(ctx) > 0 {
			return nil
		}
		ui, err := e.UI(ctx)
		if err != nil {
			return err
		}
		texts := ""
		for _, n := range ui {
			texts += n.Text + "\n"
		}
		last = strings.Join(strings.Fields(strings.ReplaceAll(texts, "\n", " | ")), " ")
		switch {
		case classify("", ui) == PromptCredential:
			if err := e.enterPIN(ctx, ui); err != nil {
				return err
			}
		case strings.Contains(texts, "Fingerprint added"):
			if n, ok := find(ui, regexp.MustCompile(`(?i)^done$`)); ok {
				e.tap(ctx, n)
			}
		default:
			if n, ok := find(ui, enrollStepRE); ok {
				e.tap(ctx, n)
			} else if err := e.FingerTouch(ctx, FingerID); err != nil {
				return err
			}
		}
		if !sleep(ctx, 1500*time.Millisecond) {
			return ctx.Err()
		}
	}
	if e.Fingerprints(ctx) > 0 {
		return nil
	}
	return fmt.Errorf("fingerprint enrollment did not finish; last screen: %.300s", last)
}
