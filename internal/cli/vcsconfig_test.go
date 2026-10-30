package cli

import (
	"fmt"
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

func TestOldBranchInheritsCanonicalQueuePolicyFromDefaultBranch(t *testing.T) {
	const policy = "[vcs]\nqueue = \"trunk\"\n[trunk]\nssm_parameter = \"/shared/queue\"\naws_profile = \"platform\"\naws_region = \"us-east-1\"\n"
	for _, tt := range []struct {
		name          string
		working       string
		hasWorking    bool
		defaultConfig string
		explicit      string
		want          string
	}{
		{name: "old branch inherits", defaultConfig: policy, want: "trunk"},
		{name: "working override", working: "[vcs]\nqueue = \"github\"\n", hasWorking: true, defaultConfig: policy, want: "github"},
		{name: "present empty disables inheritance", hasWorking: true, defaultConfig: policy, want: "github"},
		{name: "working parse error stays visible", working: "[vcs\n", hasWorking: true, defaultConfig: policy, want: "invalid"},
		{name: "default parse error stays visible", defaultConfig: "[vcs\n", want: "invalid"},
		{name: "explicit queue skips unused default", defaultConfig: "[vcs\n", explicit: "github", want: "github"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote())
			original := shipHead(t, f)
			writeShipFile(t, f.Dir, regenFile, tt.defaultConfig)
			mustRun(t, f.Env(), f.Dir, "git", "add", regenFile)
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "shared queue policy")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "old-feature", original)
			mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin", "main")
			defaultHead := gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/remotes/origin/main")
			mustRun(t, f.Env(), f.Dir, "git", "remote", "set-url", "origin", "git@github.com:org/local.git")
			if tt.hasWorking {
				writeShipFile(t, f.Dir, regenFile, tt.working)
			}
			if tt.explicit != "" {
				mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.queue", tt.explicit)
			}
			client := gtapi.NewNativeWithToken("https://example.invalid", "unused")
			previous := gtAPINativeDefault
			gtAPINativeDefault = func() *gtapi.Client { return client }
			t.Cleanup(func() { gtAPINativeDefault = previous })
			ctx := withStackProvider(f.Context(), true)
			if got := gtAPIForRepo(ctx, "org/local").QueueProvider(); got != tt.want {
				t.Fatalf("local queue=%s, want %s", got, tt.want)
			}
			if got := gtAPIForRepo(ctx, "org/foreign").QueueProvider(); got != "github" {
				t.Fatalf("foreign queue inherited default-branch policy: %s", got)
			}
			if tt.want == "trunk" {
				config, err := readRepositoryVCSConfig(ctx, f.Dir)
				if err != nil || config.Trunk != (gtapi.TrunkSSMConfig{SSMParameter: "/shared/queue", AWSProfile: "platform", AWSRegion: "us-east-1"}) {
					t.Fatalf("inherited credentials=%+v, error=%v", config.Trunk, err)
				}
				if _, err := os.Stat(filepath.Join(f.Dir, regenFile)); !os.IsNotExist(err) {
					t.Fatalf("lookup materialized working config: %v", err)
				}
			}
			if shipHead(t, f) != original || gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/remotes/origin/main") != defaultHead {
				t.Fatal("configuration lookup moved repository refs")
			}
		})
	}
}

func TestConfigurationWithoutDefaultBranchNeedsNoQueueCredentials(t *testing.T) {
	for _, removeOrigin := range []bool{false, true} {
		t.Run(fmt.Sprint(removeOrigin), func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote(), vcstest.NoOriginHead())
			if removeOrigin {
				mustRun(t, f.Env(), f.Dir, "git", "remote", "remove", "origin")
			}
			client := gtapi.NewNativeWithToken("https://example.invalid", "unused")
			previous := gtAPINativeDefault
			gtAPINativeDefault = func() *gtapi.Client { return client }
			t.Cleanup(func() { gtAPINativeDefault = previous })
			ctx := withStackProvider(f.Context(), true)
			config, err := readRepositoryVCSConfig(ctx, f.Dir)
			if err != nil || config.VCS.Queue != "" || config.Trunk != (gtapi.TrunkSSMConfig{}) {
				t.Fatalf("unexpected queue credentials/config=%+v error=%v", config, err)
			}
			if got := gtAPI(ctx).QueueProvider(); got != "github" {
				t.Fatalf("unconfigured repository queue=%s", got)
			}
		})
	}
}
