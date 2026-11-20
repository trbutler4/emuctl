package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/trbutler4/emuctl/pkg/client"
)

func TestInitializeAndToolList(t *testing.T) {
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"emulator_usage"}}`,
	}, "\n") + "\n"
	pr, pw := io.Pipe()
	go func() {
		New(&client.Client{Base: "http://127.0.0.1:1"}, "test").Serve(context.Background(), strings.NewReader(in), pw)
		pw.Close()
	}()
	got := map[string]json.RawMessage{}
	sc := bufio.NewScanner(pr)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var r struct {
			ID     json.RawMessage
			Result json.RawMessage
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		got[string(r.ID)] = r.Result
	}
	if !strings.Contains(string(got["1"]), `"version":"test"`) {
		t.Errorf("initialize: %s", got["1"])
	}
	for _, name := range []string{"emulator_acquire", "emulator_shell", "emulator_screenshot", "emulator_release"} {
		if !strings.Contains(string(got["2"]), `"`+name+`"`) {
			t.Errorf("tools/list missing %s", name)
		}
	}
	if !strings.Contains(string(got["3"]), "emulator_acquire") {
		t.Errorf("usage: %s", got["3"])
	}
}
