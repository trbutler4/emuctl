package emulator

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestClassifyBootProp(t *testing.T) {
	exitErr := exec.Command("false").Run()
	cases := []struct {
		name   string
		out    string
		err    error
		ctxErr error
		want   Health
	}{
		{"booted", "1\n", nil, nil, Healthy},
		{"not booted", "\n", nil, nil, Broken},
		{"adb exit (offline)", "error: device offline", exitErr, nil, Unknown},
		{"adb missing", "", errors.New("exec: not found"), nil, Unknown},
		{"timeout", "", exitErr, context.DeadlineExceeded, Unknown},
	}
	for _, c := range cases {
		if got, _ := classifyBootProp(c.out, c.err, c.ctxErr); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
