package codeexec

import (
	"context"
	"strings"
	"testing"
)

func TestSh(t *testing.T) {
	requireUV(t)
	tests := []struct {
		name   string
		script string
		want   string
	}{
		{"stdout", "import asyncio\nasyncio.run(sh(\"echo hello\"))", "hello\n"},
		{"nonzero exit is data", "import asyncio\nasyncio.run(sh(\"echo boom >&2; exit 3\"))", "boom\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := NewRuntime(map[string]HostFunc{"sh": Sh()})
			got, err := rt.Run(context.Background(), tt.script, 0)
			if err != nil {
				t.Fatalf("Run error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShPolicy(t *testing.T) {
	for _, cmd := range []string{"rm -rf /", "rm -r /", "dd if=/dev/zero of=x", "mkfs.ext4 /dev/sda"} {
		if shPolicy(cmd) == nil {
			t.Errorf("shPolicy(%q) = nil, want blocked", cmd)
		}
	}
	for _, cmd := range []string{"echo hi", "grep -r foo .", "rm -rf ./build", "jq '.x' f.json"} {
		if err := shPolicy(cmd); err != nil {
			t.Errorf("shPolicy(%q) = %v, want allowed", cmd, err)
		}
	}
}

func TestShBlockedRaises(t *testing.T) {
	requireUV(t)
	rt := NewRuntime(map[string]HostFunc{"sh": Sh()})
	_, err := rt.Run(context.Background(), "import asyncio\nasyncio.run(sh(\"rm -rf /\"))", 0)
	if err == nil {
		t.Fatal("Run = nil error, want policy error")
	}
	if !strings.Contains(err.Error(), "blocked by policy") {
		t.Errorf("error %q missing %q", err, "blocked by policy")
	}
}
