package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestGTCacheReusesResolvedCheckout(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Worktree("lane"))
	nested := filepath.Join(f.WorktreePath("lane"), "nested", "dir")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{f.Dir, f.WorktreePath("lane"), nested} {
		t.Run(root, func(t *testing.T) {
			checkout, err := vcs.ResolveCheckout(root)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.TrimSpace(f.Out(t, "git", "-C", root, "rev-parse", "--path-format=absolute", "--git-common-dir"))
			f.RotateLog(t)
			cache := newGTCache(render.Dir(checkout.Root), "ship", checkout)
			for range 2 {
				got, err := cache.common(f.ContextIn(root))
				if err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("common = %q, want %q", got, want)
				}
				cache.forget()
			}
			if calls := vcstest.Invocations(t, f.ArgvLog); len(calls) != 0 {
				t.Fatalf("resolved checkout spawned %v", calls)
			}
		})
	}
}

func TestGTCacheHonorsRepositoryOverrides(t *testing.T) {
	f := vcstest.Repo(t)
	other := vcstest.Repo(t)
	checkout, err := vcs.ResolveCheckout(f.Dir)
	if err != nil {
		t.Fatal(err)
	}
	otherCheckout, err := vcs.ResolveCheckout(other.Dir)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		key     string
		ambient bool
	}{
		{"context git dir", "GIT_DIR", false},
		{"context common dir", "GIT_COMMON_DIR", false},
		{"ambient common dir", "GIT_COMMON_DIR", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.ambient {
				t.Setenv(tt.key, otherCheckout.CommonDir)
			}
			ctx := func() context.Context {
				if tt.ambient {
					env := slices.DeleteFunc(f.Env(), func(entry string) bool { return strings.HasPrefix(entry, tt.key+"=") })
					return render.WithEnv(context.Background(), env...)
				}
				return render.WithEnv(f.Context(), tt.key+"="+otherCheckout.CommonDir)
			}
			want, err := gtCommonDir(ctx(), render.Dir(checkout.Root), "ship")
			if err != nil {
				t.Fatal(err)
			}
			if want != otherCheckout.CommonDir {
				t.Fatalf("git common = %q, want %q", want, otherCheckout.CommonDir)
			}
			f.RotateLog(t)
			cache := newGTCache(render.Dir(checkout.Root), "ship", checkout)
			for range 2 {
				got, err := cache.common(ctx())
				if err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("common = %q, want %q", got, want)
				}
			}
			if calls := vcstest.Invocations(t, f.ArgvLog); len(calls) != 1 {
				t.Fatalf("repository override spawned %v, want one query", calls)
			}
		})
	}
}

func TestGTCacheKeepsRelativeRepositoryOverrideFrame(t *testing.T) {
	f := vcstest.Repo(t)
	other := vcstest.Repo(t)
	checkout, err := vcs.ResolveCheckout(f.Dir)
	if err != nil {
		t.Fatal(err)
	}
	otherCheckout, err := vcs.ResolveCheckout(other.Dir)
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(f.Dir, "nested", "dir")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(nested, otherCheckout.CommonDir)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(f.Out(t, "git", "-C", nested, "--git-dir="+relative, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	if want != otherCheckout.CommonDir {
		t.Fatalf("git common = %q, want %q", want, otherCheckout.CommonDir)
	}
	f.RotateLog(t)
	ctx := render.WithEnv(f.ContextIn(nested), "GIT_DIR="+relative)
	cache := newGTCache(render.Dir(nested), "ship", checkout)
	got, err := cache.common(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("common = %q, want %q", got, want)
	}
	if calls := vcstest.Invocations(t, f.ArgvLog); len(calls) != 1 {
		t.Fatalf("relative repository override spawned %v, want one query", calls)
	}
}
