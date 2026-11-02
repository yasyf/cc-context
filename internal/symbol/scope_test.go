package symbol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/execstub"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/workspace"
)

func scopeEngines(t *testing.T, root string) (context.Context, string, string) {
	t.Helper()
	bin := t.TempDir()
	outlineLog, grepLog := filepath.Join(bin, "outline-argv"), filepath.Join(bin, "grep-argv")
	execstub.Write(t, filepath.Join(bin, "ast-grep"), "#!/bin/sh\nif [ \"$1\" = --version ]; then printf 'ast-grep 0.44.0\\n'; exit 0; fi\nprintf '%s\\n' \"$@\" > \"$CCX_OUTLINE_ARGS\"\n")
	execstub.Write(t, filepath.Join(bin, "rg"), "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CCX_GREP_ARGS\"\nexit 1\n")
	ctx := render.WithEnv(t.Context(), "PATH="+bin, "CCX_OUTLINE_ARGS="+outlineLog, "CCX_GREP_ARGS="+grepLog)
	return workspace.WithRoot(ctx, root), outlineLog, grepLog
}

func TestRunRejectsHomeScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	alias := filepath.Join(t.TempDir(), "home-alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"", home, alias} {
		for _, globs := range [][]string{nil, {"*.go"}, {"!*.md"}} {
			ctx, outlineLog, grepLog := scopeEngines(t, root)
			_, _, err := Run(ctx, backend.Args{Query: "WithScope", Globs: globs})
			if err == nil || !strings.Contains(err.Error(), "home directory") {
				t.Fatalf("root=%q globs=%q: expected home scope refusal, got %v", root, globs, err)
			}
			for _, log := range []string{outlineLog, grepLog} {
				if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("home scope started engine %q: %v", log, err)
				}
			}
		}
	}
}

func TestRunMissingScopeDoesNotSearchHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, "missing.go")
	ctx, outlineLog, grepLog := scopeEngines(t, home)
	_, _, err := Run(ctx, backend.Args{Query: "WithScope", Body: true, Globs: []string{target}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing symbol error=%v", err)
	}
	data, err := os.ReadFile(outlineLog)
	if err != nil {
		t.Fatal(err)
	}
	outline := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(outline) < 2 || outline[0] != "outline" || outline[1] != target {
		t.Fatalf("outline argv=%q, want literal scope %q", outline, target)
	}
	data, err = os.ReadFile(grepLog)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(argv) < 2 || argv[len(argv)-2] != "--" || argv[len(argv)-1] != target {
		t.Fatalf("missing scope widened: argv=%q, want literal operand %q", argv, target)
	}
}

func TestRunScopePreservesBoundedLookups(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	source := filepath.Join(home, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(source, "target.go")
	if err := os.WriteFile(file, []byte("package source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		root    string
		globs   []string
		operand string
	}{
		{project, nil, ""},
		{home, []string{source + "/**"}, source},
		{home, []string{file}, source},
	} {
		ctx, _, grepLog := scopeEngines(t, tc.root)
		_, _, err := Run(ctx, backend.Args{Query: "WithScope", Globs: tc.globs})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("bounded root=%q globs=%q: %v", tc.root, tc.globs, err)
		}
		data, err := os.ReadFile(grepLog)
		if err != nil {
			t.Fatal(err)
		}
		argv := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		if tc.operand != "" && (len(argv) < 2 || argv[len(argv)-2] != "--" || argv[len(argv)-1] != tc.operand) {
			t.Fatalf("bounded lookup argv=%q, want %q", argv, tc.operand)
		}
	}
}
