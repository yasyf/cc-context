package cli

import (
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestShipSweepHoldsBackUntrackedMachineLocalFiles(t *testing.T) {
	for _, gt := range []bool{false, true} {
		name := "git"
		if gt {
			name = "graphite"
		}
		t.Run(name, func(t *testing.T) {
			var f *vcstest.Fixture
			if gt {
				f = shipGTRepo(t, vcstest.GTStack("base"))
			} else {
				f = shipRepo(t, vcstest.Remote())
			}
			writeShipFile(t, f.Dir, "tracked.txt", "one\n")
			writeShipFile(t, f.Dir, ".env.example", "KEY=\n")
			mustRun(t, f.Env(), f.Dir, "git", "add", "tracked.txt", ".env.example")
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "tracked")
			writeShipFile(t, f.Dir, "tracked.txt", "two\n")
			writeShipFile(t, f.Dir, ".env.example", "KEY=example\n")
			writeShipFile(t, f.Dir, "new.txt", "new\n")
			local := []string{".claude/settings.local.json", ".env", "mise.local.toml"}
			for _, path := range local {
				writeShipFile(t, f.Dir, path, "mine\n")
			}

			got, err := runShipCmd(f.Context(), t, "-m", "fix: the sweep", "--no-push")
			if err != nil {
				t.Fatalf("ship = %v", err)
			}
			if want := "held 3 machine-local path(s): .claude/settings.local.json, .env, mise.local.toml — name them to ship them"; !strings.Contains(got, want) {
				t.Errorf("summary = %q, want it to contain %q", got, want)
			}
			if files := gitAt(t, f.Env(), f.Dir, "show", "--name-only", "--format=", "HEAD"); files != ".env.example\nnew.txt\ntracked.txt" {
				t.Errorf("committed files = %q, want the tracked edits and new.txt only", files)
			}
			for _, path := range local {
				if status := gitAt(t, f.Env(), f.Dir, "status", "--porcelain", "--untracked-files=all", "--", path); status != "?? "+path {
					t.Errorf("%s status = %q, want it left untracked", path, status)
				}
			}

			if _, err := runShipCmd(f.Context(), t, "-m", "chore: pin the local mise config", "--no-push", "mise.local.toml"); err != nil {
				t.Fatalf("named ship = %v", err)
			}
			if files := gitAt(t, f.Env(), f.Dir, "show", "--name-only", "--format=", "HEAD"); files != "mise.local.toml" {
				t.Errorf("committed files = %q, want the named mise.local.toml", files)
			}
		})
	}
}

func TestMachineLocalPath(t *testing.T) {
	for path, want := range map[string]bool{
		"mise.local.toml":             true,
		".claude/settings.local.json": true,
		".env":                        true,
		"web/.env.local":              true,
		".envrc":                      true,
		"local.txt":                   false,
		"docs/locale.md":              false,
		"environment.go":              false,
		"src/env/config.go":           false,
	} {
		if got := machineLocalPath(path); got != want {
			t.Errorf("machineLocalPath(%q) = %v, want %v", path, got, want)
		}
	}
}
