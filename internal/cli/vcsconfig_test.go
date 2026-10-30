package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestTrackedQueueSelectionRespectsRepositoryAndExplicitOverride(t *testing.T) {
	for _, tt := range []struct {
		name     string
		file     string
		git      string
		override bool
		want     string
	}{
		{name: "no repo configuration", want: "github"},
		{name: "tracked trunk default", file: "[vcs]\nqueue = \"trunk\"\n", want: "trunk"},
		{name: "explicit github override", file: "[vcs]\nqueue = \"trunk\"\n", git: "github", override: true, want: "github"},
		{name: "explicit provider default", file: "[vcs]\nqueue = \"trunk\"\n", override: true, want: "github"},
		{name: "invalid tracked queue", file: "[vcs]\nqueue = \"unexpected\"\n", want: "invalid"},
		{name: "unused malformed tracked config", file: "[vcs\n", git: "github", override: true, want: "github"},
		{name: "malformed tracked config", file: "[vcs\n", want: "invalid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote())
			mustRun(t, f.Env(), f.Dir, "git", "remote", "set-url", "origin", "git@github.com:org/local.git")
			writeShipFile(t, f.Dir, ".ccx.toml", "legacy root must not be read by the canonical loader")
			if tt.file != "" {
				writeShipFile(t, f.Dir, regenFile, tt.file)
			}
			if tt.override {
				mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.queue", tt.git)
			}
			client := gtapi.NewNativeWithToken("https://example.invalid", "unused")
			previous := gtAPINativeDefault
			gtAPINativeDefault = func() *gtapi.Client { return client }
			t.Cleanup(func() { gtAPINativeDefault = previous })
			dir := filepath.Join(f.Dir, "nested")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			ctx := withStackProvider(f.ContextIn(dir), true)
			if got := gtAPIForRepo(ctx, "org/local").QueueProvider(); got != tt.want {
				t.Fatalf("local queue=%s, want %s", got, tt.want)
			}
			if got := gtAPIForRepo(ctx, "org/foreign").QueueProvider(); got != "github" {
				t.Fatalf("foreign queue inherited local configuration: %s", got)
			}
		})
	}
}
