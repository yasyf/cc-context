package locate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// goScript is a fake `go` that answers `env GOMODCACHE` from $FAKE_GOMODCACHE and
// `list` from $FAKE_GOLIST (exiting nonzero when unset, mimicking a name that is
// not a module in the current context).
const goScript = `#!/bin/sh
case "$1" in
env) printf '%s\n' "$FAKE_GOMODCACHE" ;;
list)
  if [ -n "$FAKE_GOLIST" ]; then printf '%s\n' "$FAKE_GOLIST"; else exit 1; fi ;;
*) exit 1 ;;
esac
`

// pyScript is a fake `python3` that echoes "$FAKE_PYPATH\t$FAKE_PYVERSION",
// standing in for the importlib package-path + version probe.
const pyScript = `#!/bin/sh
printf '%s\t%s\n' "$FAKE_PYPATH" "$FAKE_PYVERSION"
`

func TestLocate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}

	tests := []struct {
		name       string
		repos      []string
		withGo     bool
		goList     func(cache string) string
		cacheMods  []string
		withPython bool
		pyPath     string
		pyVersion  string
		query      string
		want       func(ws, cache string) []Result
	}{
		{
			name:  "exact repo match wins",
			repos: []string{"captain-hook", "cc-context", "zzz"},
			query: "cc-context",
			want: func(ws, _ string) []Result {
				return []Result{{Kind: KindRepo, Path: filepath.Join(ws, "cc-context")}}
			},
		},
		{
			name:  "case-insensitive substring matches",
			repos: []string{"CC-Context", "cc-pushback", "other"},
			query: "cc",
			want: func(ws, _ string) []Result {
				return []Result{
					{Kind: KindRepo, Path: filepath.Join(ws, "CC-Context")},
					{Kind: KindRepo, Path: filepath.Join(ws, "cc-pushback")},
				}
			},
		},
		{
			name:  "substring matches capped at ten",
			repos: manyRepos("match", 12),
			query: "match",
			want: func(ws, _ string) []Result {
				out := make([]Result, 0, substringCap)
				for _, d := range manyRepos("match", 12)[:substringCap] {
					out = append(out, Result{Kind: KindRepo, Path: filepath.Join(ws, d)})
				}
				return out
			},
		},
		{
			name:  "normalized query matches hyphen repo dir",
			repos: []string{"cc-transcript", "other"},
			query: "cc_transcript",
			want: func(ws, _ string) []Result {
				return []Result{{Kind: KindRepo, Path: filepath.Join(ws, "cc-transcript")}}
			},
		},
		{
			name:   "go list hit",
			withGo: true,
			goList: func(_ string) string { return "/fake/mod/dir@v2.0.0" },
			query:  "example.com/mod",
			want: func(_, _ string) []Result {
				return []Result{{Kind: KindGoModule, Path: "/fake/mod/dir", Version: "v2.0.0"}}
			},
		},
		{
			name:      "module cache newest three",
			withGo:    true,
			cacheMods: []string{"v1.7.0", "v1.8.0", "v1.9.0", "v1.10.0"},
			query:     "cobra",
			want: func(_, cache string) []Result {
				return []Result{
					{Kind: KindGoModule, Path: filepath.Join(cache, "cobra@v1.10.0"), Version: "v1.10.0"},
					{Kind: KindGoModule, Path: filepath.Join(cache, "cobra@v1.9.0"), Version: "v1.9.0"},
					{Kind: KindGoModule, Path: filepath.Join(cache, "cobra@v1.8.0"), Version: "v1.8.0"},
				}
			},
		},
		{
			name:      "go list result dedupes cache duplicate",
			withGo:    true,
			goList:    func(cache string) string { return filepath.Join(cache, "cobra@v1.9.0") + "@v1.9.0" },
			cacheMods: []string{"v1.8.0", "v1.9.0", "v1.10.0"},
			query:     "cobra",
			want: func(_, cache string) []Result {
				return []Result{
					{Kind: KindGoModule, Path: filepath.Join(cache, "cobra@v1.9.0"), Version: "v1.9.0"},
					{Kind: KindGoModule, Path: filepath.Join(cache, "cobra@v1.10.0"), Version: "v1.10.0"},
					{Kind: KindGoModule, Path: filepath.Join(cache, "cobra@v1.8.0"), Version: "v1.8.0"},
				}
			},
		},
		{
			name:       "package resolves with version",
			withPython: true,
			pyPath:     "/py/site-packages/foo",
			pyVersion:  "1.2.3",
			query:      "foo",
			want: func(_, _ string) []Result {
				return []Result{{Kind: KindPackage, Path: "/py/site-packages/foo", Version: "1.2.3"}}
			},
		},
		{
			name:       "package resolves without version",
			withPython: true,
			pyPath:     "/py/site-packages/foo",
			query:      "foo",
			want: func(_, _ string) []Result {
				return []Result{{Kind: KindPackage, Path: "/py/site-packages/foo"}}
			},
		},
		{
			name:       "repo and package resolve, repo first",
			repos:      []string{"foo"},
			withPython: true,
			pyPath:     "/py/site-packages/foo",
			pyVersion:  "9.9.9",
			query:      "foo",
			want: func(ws, _ string) []Result {
				return []Result{
					{Kind: KindRepo, Path: filepath.Join(ws, "foo")},
					{Kind: KindPackage, Path: "/py/site-packages/foo", Version: "9.9.9"},
				}
			},
		},
		{
			name:       "absent python and go contribute nothing",
			repos:      []string{"foo"},
			withPython: false,
			query:      "foo",
			want: func(ws, _ string) []Result {
				return []Result{{Kind: KindRepo, Path: filepath.Join(ws, "foo")}}
			},
		},
		{
			name:       "all three resolvers merge in order",
			repos:      []string{"cobra"},
			withGo:     true,
			goList:     func(_ string) string { return "/fake/mod/dir@v2.0.0" },
			withPython: true,
			pyPath:     "/py/site-packages/foo",
			pyVersion:  "4.5.6",
			query:      "cobra",
			want: func(ws, _ string) []Result {
				return []Result{
					{Kind: KindRepo, Path: filepath.Join(ws, "cobra")},
					{Kind: KindGoModule, Path: "/fake/mod/dir", Version: "v2.0.0"},
					{Kind: KindPackage, Path: "/py/site-packages/foo", Version: "4.5.6"},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := t.TempDir()
			for _, d := range tt.repos {
				mustMkdir(t, filepath.Join(ws, d))
			}

			binDir := t.TempDir()
			cache := t.TempDir()
			if tt.withGo {
				writeScript(t, binDir, "go", goScript)
				t.Setenv("FAKE_GOMODCACHE", cache)
				goList := ""
				if tt.goList != nil {
					goList = tt.goList(cache)
				}
				t.Setenv("FAKE_GOLIST", goList)
				for _, v := range tt.cacheMods {
					mustMkdir(t, filepath.Join(cache, encodeModulePath(tt.query)+"@"+v))
				}
			}
			if tt.withPython {
				writeScript(t, binDir, "python3", pyScript)
				t.Setenv("FAKE_PYPATH", tt.pyPath)
				t.Setenv("FAKE_PYVERSION", tt.pyVersion)
			}
			t.Setenv("VIRTUAL_ENV", "") // a dev's active venv must not shadow the fake python3
			t.Setenv("PATH", binDir)

			got, err := Locate(context.Background(), tt.query, ws)
			if err != nil {
				t.Fatalf("Locate() error = %v", err)
			}
			want := tt.want(ws, cache)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Locate() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestLocateMissingWorkspaceIsNoError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got, err := Locate(context.Background(), "anything", filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("Locate() error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("Locate() = %#v, want no results", got)
	}
}

// TestDedupePreservesKindAtSharedPath proves an editable install — a repo row and
// a package row resolving to the same directory — keeps both rows because the key
// carries the kind, while a same-kind same-path duplicate (a go list hit that also
// appears in the module cache) still collapses.
func TestDedupePreservesKindAtSharedPath(t *testing.T) {
	shared := "/Users/dev/Code/cc-transcript"
	in := []Result{
		{Kind: KindRepo, Path: shared},
		{Kind: KindGoModule, Path: "/cache/mod@v1.0.0", Version: "v1.0.0"},
		{Kind: KindGoModule, Path: "/cache/mod@v1.0.0", Version: "v1.0.0"},
		{Kind: KindPackage, Path: shared, Version: "10.0.0"},
	}
	got := dedupe(in)
	want := []Result{
		{Kind: KindRepo, Path: shared},
		{Kind: KindGoModule, Path: "/cache/mod@v1.0.0", Version: "v1.0.0"},
		{Kind: KindPackage, Path: shared, Version: "10.0.0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dedupe() = %#v, want %#v", got, want)
	}
}

func TestResolvePythonPrefersVenv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}
	dir := t.TempDir()
	t.Chdir(dir)

	venvBin := filepath.Join(dir, ".venv", "bin")
	mustMkdir(t, venvBin)
	writeScript(t, venvBin, "python3", "#!/bin/sh\nprintf '%s\\t%s\\n' /venv/site-packages/foo 1.0.0\n")

	pathBin := t.TempDir()
	writeScript(t, pathBin, "python3", "#!/bin/sh\nprintf '%s\\t%s\\n' /path/site-packages/foo 2.0.0\n")
	t.Setenv("VIRTUAL_ENV", "")
	t.Setenv("PATH", pathBin)

	got := resolvePython(context.Background(), "foo")
	want := []Result{{Kind: KindPackage, Path: "/venv/site-packages/foo", Version: "1.0.0"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolvePython() = %#v, want %#v", got, want)
	}
}

func TestEncodeModulePath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"github.com/spf13/cobra", "github.com/spf13/cobra"},
		{"github.com/Azure/azure-sdk-for-go", "github.com/!azure/azure-sdk-for-go"},
		{"github.com/BurntSushi/toml", "github.com/!burnt!sushi/toml"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := encodeModulePath(tt.in); got != tt.want {
				t.Errorf("encodeModulePath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"minor beats patch lexical", "v1.10.0", "v1.9.0", 1},
		{"equal", "v1.2.3", "v1.2.3", 0},
		{"older", "v1.2.3", "v1.2.4", -1},
		{"release beats prerelease", "v1.2.3", "v1.2.3-beta", 1},
		{"prerelease ordering", "v1.2.3-alpha", "v1.2.3-beta", -1},
		{"build metadata ignored", "v1.2.3+meta", "v1.2.3", 0},
		{"major", "v2.0.0", "v1.99.99", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sign(compareVersions(tt.a, tt.b)); got != tt.want {
				t.Errorf("compareVersions(%q, %q) sign = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func manyRepos(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix + string(rune('a'+i))
	}
	return out
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
}

func writeScript(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec // fake executable must be owner-executable
		t.Fatalf("write fake %q: %v", name, err)
	}
}
