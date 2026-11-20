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
