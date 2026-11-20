package client

import "testing"

func TestParseSSH(t *testing.T) {
	for in, want := range map[string]sshTarget{
		"ssh://host":                {dest: "host", remote: "127.0.0.1:7600"},
		"ssh://me@host:2222":        {dest: "me@host", port: "2222", remote: "127.0.0.1:7600"},
		"ssh://host/127.0.0.1:7700": {dest: "host", remote: "127.0.0.1:7700"},
		"ssh://me@host/[::1]:7600":  {dest: "me@host", remote: "[::1]:7600"},
	} {
		got, err := parseSSH(in)
		if err != nil || *got != want {
			t.Errorf("%s: got %+v %v, want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"ssh://", "ssh://host/nope"} {
		if _, err := parseSSH(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestBadSSHURLFailsRequests(t *testing.T) {
	t.Setenv("EMUCTL_URL", "ssh://")
	if _, err := FromEnv().Devices(t.Context()); err == nil {
		t.Fatal("bad ssh URL must fail")
	}
}
