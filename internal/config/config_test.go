package config

import (
	"strings"
	"testing"
)

func TestParsePortRange(t *testing.T) {
	good := map[string]PortRange{"1024-65535": {1024, 65535}, "8000": {8000, 8000}, " 1-2 ": {1, 2}}
	for in, want := range good {
		got, err := ParsePortRange(in)
		if err != nil || got != want {
			t.Errorf("%q: got %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0-10", "10-5", "1-70000", "a-b", "-"} {
		if _, err := ParsePortRange(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestParseBind(t *testing.T) {
	if _, lo, err := ParseBind("127.0.0.1"); err != nil || !lo {
		t.Error("127.0.0.1")
	}
	if _, lo, err := ParseBind("[::1]"); err != nil || !lo {
		t.Error("::1")
	}
	if _, lo, err := ParseBind("0.0.0.0"); err != nil || lo {
		t.Error("0.0.0.0 must be accepted but not loopback")
	}
	for _, in := range []string{"localhost", "", "1.2.3", "example.com"} {
		if _, _, err := ParseBind(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestValidateSocketPath(t *testing.T) {
	if ValidateSocketPath("/run/xvia/agent.sock") != nil {
		t.Error("valid path rejected")
	}
	for _, p := range []string{"", "relative.sock", "/" + strings.Repeat("a", 200)} {
		if ValidateSocketPath(p) == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestHostSocketPathUsesXDG(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/4242")
	if got := HostSocketPath(); got != "/run/user/4242/xvia/agent.sock" {
		t.Fatal(got)
	}
}
