package mcpserver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFlightDelta(t *testing.T) {
	tests := []struct {
		name string
		line string
		want int64
	}{
		{"call", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{}}`, 1},
		{"server call", `{"jsonrpc":"2.0","id":"s1","method":"roots/list"}`, 1},
		{"result", `{"jsonrpc":"2.0","id":3,"result":{}}`, -1},
		{"error", `{"jsonrpc":"2.0","id":3,"error":{"code":-1,"message":"x"}}`, -1},
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, 0},
		{"blank", ``, 0},
		{"garbage", `not json`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := flightDelta([]byte(tt.line)); got != tt.want {
				t.Errorf("flightDelta(%q) = %d, want %d", tt.line, got, tt.want)
			}
		})
	}
}

func TestVersionGreater(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"0.96.27", "0.96.26", true},
		{"0.96.26", "0.96.26", false},
		{"0.96.9", "0.96.10", false},
		{"1.0", "0.99.99", true},
		{"0.97", "0.96.30", true},
	}
	for _, tt := range tests {
		if got := versionGreater(tt.a, tt.b); got != tt.want {
			t.Errorf("versionGreater(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestNewestInstall(t *testing.T) {
	cache := t.TempDir()
	for _, v := range []string{"0.96.25", "0.96.26", "0.96.30", "0.96.9", "scratch"} {
		bin := filepath.Join(cache, v, "bin")
		if err := os.MkdirAll(bin, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "ccx"), []byte("#!/bin/sh\n"), 0o700); err != nil { // #nosec G306 -- the test launcher must be executable
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cache, "0.96.30", orphanedMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(pluginRootEnv, filepath.Join(cache, "0.96.25"))
	got, dir := newestInstall()
	if got != "0.96.26" || dir != filepath.Join(cache, "0.96.26") {
		t.Errorf("newestInstall() = %q, %q; want 0.96.26 beside the orphaned 0.96.30", got, dir)
	}
}
