package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestGeneratedConfigMigrationPrecedence(t *testing.T) {
	const legacy = "[[generated]]\npaths = [\"gen/**\"]\nrun = \"legacy from repository root\"\n"
	const canonical = "[[generated]]\npaths = [\"gen/**\"]\nrun = \"canonical from repository root\"\n"
	for _, tt := range []struct {
		name      string
		files     map[string]string
		want      string
		errorPath string
	}{
		{name: "absent"},
		{name: "old branch", files: map[string]string{legacyRegenFile: legacy}, want: "legacy from repository root"},
		{name: "new branch retains old client data", files: map[string]string{legacyRegenFile: legacy, regenFile: canonical}, want: "canonical from repository root"},
		{name: "canonical only", files: map[string]string{regenFile: canonical}, want: "canonical from repository root"},
		{name: "empty canonical", files: map[string]string{legacyRegenFile: legacy, regenFile: ""}},
		{name: "empty canonical suppresses invalid legacy", files: map[string]string{legacyRegenFile: "[invalid", regenFile: ""}},
		{name: "malformed canonical", files: map[string]string{legacyRegenFile: legacy, regenFile: "[invalid"}, errorPath: regenFile},
		{name: "unknown canonical key", files: map[string]string{legacyRegenFile: legacy, regenFile: "unknown = true"}, errorPath: regenFile},
		{name: "malformed legacy", files: map[string]string{legacyRegenFile: "[invalid"}, errorPath: legacyRegenFile},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.Repo(t)
			for name, body := range tt.files {
				writeShipFile(t, f.Dir, name, body)
			}
			mustRun(t, f.Env(), f.Dir, "git", "add", "-A")
			mustRun(t, f.Env(), f.Dir, "git", "commit", "--allow-empty", "-qm", "configuration")
			for _, reader := range []string{"revision", "index"} {
				t.Run(reader, func(t *testing.T) {
					var generators []regenGenerator
					var err error
					if reader == "revision" {
						generators, err = regenLoad(f.Context(), render.Dir(f.Dir), "HEAD")
					} else {
						generators, err = regenStaged(f.Context(), render.Dir(f.Dir))
					}
					if tt.errorPath != "" {
						if err == nil || !strings.Contains(err.Error(), tt.errorPath) {
							t.Fatalf("error=%v, want selected %s refusal", err, tt.errorPath)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if tt.want == "" {
						if len(generators) != 0 {
							t.Fatalf("unexpected definitions: %+v", generators)
						}
						return
					}
					if len(generators) != 1 || generators[0].Run != tt.want || !generators[0].owns("gen/output.txt") || generators[0].owns(".claude/gen/output.txt") {
						t.Fatalf("repository-root definitions changed: %+v", generators)
					}
				})
			}
			if _, present := tt.files[legacyRegenFile]; present {
				if got := gitAt(t, f.Env(), f.Dir, "show", "HEAD:"+legacyRegenFile); got != strings.TrimSpace(tt.files[legacyRegenFile]) {
					t.Fatal("migration changed the data released root readers consume")
				}
			}
		})
	}
}

func TestGeneratedConfigReadsSelectedRevisionAndIndex(t *testing.T) {
	f := vcstest.Repo(t)
	const legacy = "[[generated]]\npaths=[\"gen/**\"]\nrun=\"legacy\"\n"
	const canonical = "[[generated]]\npaths=[\"gen/**\"]\nrun=\"canonical\"\n"
	writeShipFile(t, f.Dir, legacyRegenFile, legacy)
	mustRun(t, f.Env(), f.Dir, "git", "add", legacyRegenFile)
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "old branch")
	old := shipHead(t, f)
	writeShipFile(t, f.Dir, regenFile, canonical)
	mustRun(t, f.Env(), f.Dir, "git", "add", regenFile)
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "new branch")
	writeShipFile(t, f.Dir, regenFile, "[unstaged invalid")
	for revision, want := range map[string]string{old: "legacy", "HEAD": "canonical"} {
		got, err := regenLoad(f.Context(), render.Dir(f.Dir), revision)
		if err != nil || len(got) != 1 || got[0].Run != want {
			t.Fatalf("revision %s=%+v,%v", revision, got, err)
		}
	}
	got, err := regenStaged(f.Context(), render.Dir(f.Dir))
	if err != nil || len(got) != 1 || got[0].Run != "canonical" {
		t.Fatalf("index ignored staged canonical: %+v,%v", got, err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "rm", "-q", "--cached", regenFile)
	got, err = regenStaged(f.Context(), render.Dir(f.Dir))
	if err != nil || len(got) != 1 || got[0].Run != "legacy" {
		t.Fatalf("index ignored canonical absence: %+v,%v", got, err)
	}
	if _, err := regenLoad(f.Context(), render.Dir(f.Dir), "refs/heads/missing-revision"); err == nil {
		t.Fatal("failed tree read used root definitions")
	}
}

func TestGeneratedConfigDoesNotFallBackFromConflictedIndex(t *testing.T) {
	for _, path := range []string{regenFile, legacyRegenFile} {
		t.Run(path, func(t *testing.T) {
			f := vcstest.Repo(t)
			const raw = "[[generated]]\npaths=[\"gen/**\"]\nrun=\"true\"\n"
			writeShipFile(t, f.Dir, legacyRegenFile, raw)
			writeShipFile(t, f.Dir, regenFile, raw)
			mustRun(t, f.Env(), f.Dir, "git", "add", "-A")
			oid := gitAt(t, f.Env(), f.Dir, "rev-parse", ":"+path)
			mustRun(t, f.Env(), f.Dir, "git", "update-index", "--force-remove", regenFile)
			mustRun(t, f.Env(), f.Dir, "git", "update-index", "--force-remove", path)
			stages := fmt.Sprintf("100644 %s 1\t%s\n100644 %s 2\t%s\n", oid, path, oid, path)
			if _, err := render.RunCLIStdin(f.Context(), render.Dir(f.Dir), "git", []string{"update-index", "--index-info"}, []byte(stages)); err != nil {
				t.Fatal(err)
			}
			_, err := regenStaged(f.Context(), render.Dir(f.Dir))
			if err == nil || !strings.Contains(err.Error(), path+" is itself conflicted") {
				t.Fatalf("conflicted index selected another file: %v", err)
			}
		})
	}
}
