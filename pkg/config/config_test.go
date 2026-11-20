package config

import (
	"strings"
	"testing"
)

func TestDefaultsAndAuth(t *testing.T) {
	c, err := Parse([]byte(`{"users":{"alice":"` + HashToken("secret") + `"},"devices":{"b":{},"a":{"expose_port":15601}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Devices["a"].ConsolePort != 5600 || c.Devices["b"].ConsolePort != 5602 {
		t.Fatalf("ports: %+v", c.Devices)
	}
	if u, ok := c.Authenticate("secret"); !ok || u != "alice" {
		t.Fatal("auth failed")
	}
	if _, ok := c.Authenticate("nope"); ok {
		t.Fatal("bad token accepted")
	}
	if _, ok := c.Authenticate(""); ok {
		t.Fatal("empty token accepted")
	}
}

func TestValidation(t *testing.T) {
	for _, bad := range []string{
		`{"devices":{"a":{"console_port":5601}}}`,
		`{"devices":{"a":{"console_port":5600},"b":{"console_port":5600}}}`,
		`{"devices":{"a":{"console_port":5600,"expose_port":5601}}}`,
		`{"devices":{"Bad":{}}}`,
		`{"users":{"u":"plaintext"},"devices":{}}`,
		`{"lease_ttl":"soon","devices":{}}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestExampleParses(t *testing.T) {
	if _, err := Parse([]byte(Example)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(Example, "expose_port") {
		t.Fatal("example should show expose_port")
	}
}

func TestOpenModeLoopbackOnly(t *testing.T) {
	if c, err := Parse([]byte(`{"devices":{"a":{}}}`)); err != nil || !c.Open() {
		t.Fatalf("default loopback open mode: %v", err)
	}
	for _, bad := range []string{
		`{"api":"0.0.0.0:7600","devices":{}}`,
		`{"listen":"100.64.0.1","devices":{}}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("open mode on non-loopback accepted: %s", bad)
		}
	}
	ok := `{"api":"0.0.0.0:7600","listen":"0.0.0.0","users":{"a":"` + HashToken("x") + `"},"devices":{}}`
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseConfig(t *testing.T) {
	base := func(extra string) error {
		_, err := Parse([]byte(`{"sdk":"/x",` + extra + `"devices":{"p1":{}}}`))
		return err
	}
	for extra, ok := range map[string]bool{
		``:                   true,
		`"release":"wipe",`:  true,
		`"release":"clear",`: false, // needs packages
		`"release":"clear","release_packages":["com.example.app"],`: true,
		`"release":"clear","release_packages":["not a pkg"],`:       false,
		`"release":"reboot",`:     false,
		`"health_interval":"0",`:  true,
		`"health_interval":"1s",`: false,
		`"lease_ttl":"0s",`:       false,
		`"lease_ttl":"5h",`:       false, // above max_lease_ttl
	} {
		if err := base(extra); (err == nil) != ok {
			t.Errorf("%s: err=%v, want ok=%v", extra, err, ok)
		}
	}
	c, _ := Parse([]byte(`{"sdk":"/x","reset_on_release":true,"devices":{}}`))
	if c.Release != ReleaseWipe || !*c.DisableAnimations {
		t.Fatalf("legacy reset_on_release must map to wipe: %+v", c)
	}
}
