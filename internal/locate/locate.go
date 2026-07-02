// Package locate resolves a name to on-disk paths across sibling workspace
// repos, Go modules, and Python packages.
package locate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Kind labels the resolver a Result came from.
type Kind string

const (
	// KindRepo is a sibling repository under the workspace root.
	KindRepo Kind = "repo"
	// KindGoModule is a Go module resolved via the build context or module cache.
	KindGoModule Kind = "gomod"
	// KindPython is a Python package resolved via importlib.
	KindPython Kind = "python"
)

// substringCap bounds the case-insensitive fallback matches a workspace search
// returns when no directory name matches exactly.
const substringCap = 10

// cacheVersions bounds how many module-cache versions the Go resolver reports.
const cacheVersions = 3

// pythonProbe prints the resolved spec origin of its argument, or an empty line
// when the package does not resolve.
const pythonProbe = `import importlib.util,sys; s=importlib.util.find_spec(sys.argv[1]); print(s.origin if s else '')`

// Result is a single resolved on-disk location for a queried name.
type Result struct {
	Kind    Kind
	Path    string
	Version string
}

// Locate resolves name across the workspace repos, Go modules, and Python
// packages, returning every distinct on-disk hit in resolver order. Expected
// misses — an absent workspace, a name that is not a dependency, a missing
// python3 — contribute no results and are not errors; only an unexpected
// filesystem failure reading the workspace is returned.
func Locate(ctx context.Context, name, workspace string) ([]Result, error) {
	repos, err := resolveWorkspace(workspace, name)
	if err != nil {
		return nil, err
	}

	results := repos
	results = append(results, resolveGoModule(ctx, name)...)
	results = append(results, resolvePython(ctx, name)...)
	return dedupe(results), nil
}

// resolveWorkspace matches name against the immediate children of the workspace
// root: an exact directory name wins outright, otherwise up to substringCap
// case-insensitive substring matches. A missing workspace is no match; any other
// read failure is returned.
func resolveWorkspace(workspace, name string) ([]Result, error) {
	entries, err := os.ReadDir(workspace)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read workspace %q: %w", workspace, err)
	}

	for _, e := range entries {
		if e.IsDir() && e.Name() == name {
			return []Result{{Kind: KindRepo, Path: filepath.Join(workspace, name)}}, nil
		}
	}

	needle := strings.ToLower(name)
	var results []Result
	for _, e := range entries {
		if !e.IsDir() || !strings.Contains(strings.ToLower(e.Name()), needle) {
			continue
		}
		results = append(results, Result{Kind: KindRepo, Path: filepath.Join(workspace, e.Name())})
		if len(results) == substringCap {
			break
		}
	}
	return results, nil
}

// resolveGoModule resolves name as a Go module: the build context's own view via
// `go list -m`, then the newest module-cache versions under GOMODCACHE. Both are
// best-effort — a name that is no module, or an absent go toolchain, yields no
// results rather than an error.
func resolveGoModule(ctx context.Context, name string) []Result {
	var results []Result
	if r, ok := goListModule(ctx, name); ok {
		results = append(results, r)
	}
	return append(results, goModCacheVersions(ctx, name)...)
}

// goListModule reports name's directory and version in the current module
// context. A nonzero exit means name is not a module here — a miss, not an error.
func goListModule(ctx context.Context, name string) (Result, bool) {
	out, err := exec.CommandContext(ctx, "go", "list", "-m", "-f", "{{.Dir}}@{{.Version}}", name).Output() //nolint:gosec // fixed go argv; only the module name varies
	if err != nil {
		return Result{}, false
	}
	line := strings.TrimSpace(string(out))
	at := strings.LastIndexByte(line, '@')
	if at < 0 {
		return Result{}, false
	}
	dir := line[:at]
	if dir == "" {
		return Result{}, false
	}
	return Result{Kind: KindGoModule, Path: dir, Version: line[at+1:]}, true
}

// goModCacheVersions globs the encoded module path under GOMODCACHE and returns
// the cacheVersions newest downloaded versions, newest first.
func goModCacheVersions(ctx context.Context, name string) []Result {
	out, err := exec.CommandContext(ctx, "go", "env", "GOMODCACHE").Output() //nolint:gosec // fixed go argv
	if err != nil {
		return nil
	}
	cache := strings.TrimSpace(string(out))
	if cache == "" {
		return nil
	}

	matches, err := filepath.Glob(filepath.Join(cache, encodeModulePath(name)) + "@*")
	if err != nil {
		return nil
	}
	sort.Slice(matches, func(i, j int) bool {
		return compareVersions(versionOf(matches[i]), versionOf(matches[j])) > 0
	})
	if len(matches) > cacheVersions {
		matches = matches[:cacheVersions]
	}

	results := make([]Result, 0, len(matches))
	for _, m := range matches {
		results = append(results, Result{Kind: KindGoModule, Path: m, Version: versionOf(m)})
	}
	return results
}

// resolvePython resolves name to the origin of its importlib spec. An absent
// python3 or an unresolvable package contributes nothing.
func resolvePython(ctx context.Context, name string) []Result {
	if _, err := exec.LookPath("python3"); err != nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "python3", "-c", pythonProbe, name).Output() //nolint:gosec // fixed probe; only the package name varies
	if err != nil {
		return nil
	}
	origin := strings.TrimSpace(string(out))
	if origin == "" {
		return nil
	}
	return []Result{{Kind: KindPython, Path: origin}}
}

// encodeModulePath applies Go's module-cache case encoding: every uppercase
// letter becomes an exclamation mark followed by its lowercase form.
func encodeModulePath(mod string) string {
	var b strings.Builder
	for _, r := range mod {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// versionOf returns the version segment of a module-cache path (the text after
// the final '@').
func versionOf(path string) string {
	if at := strings.LastIndexByte(path, '@'); at >= 0 {
		return path[at+1:]
	}
	return ""
}

// compareVersions orders two Go module version strings, returning a negative,
// zero, or positive result as a is older than, equal to, or newer than b. It
// compares the leading dotted numeric core numerically and treats a release as
// newer than any prerelease of the same core.
func compareVersions(a, b string) int {
	ac, apre := splitVersion(a)
	bc, bpre := splitVersion(b)
	for i := 0; i < len(ac) || i < len(bc); i++ {
		var an, bn int
		if i < len(ac) {
			an = ac[i]
		}
		if i < len(bc) {
			bn = bc[i]
		}
		if an != bn {
			return an - bn
		}
	}
	switch {
	case apre == bpre:
		return 0
	case apre == "":
		return 1
	case bpre == "":
		return -1
	case apre < bpre:
		return -1
	default:
		return 1
	}
}

// splitVersion parses a Go module version into its dotted numeric core and its
// prerelease suffix, dropping any build metadata.
func splitVersion(v string) ([]int, string) {
	v = strings.TrimPrefix(v, "v")
	if plus := strings.IndexByte(v, '+'); plus >= 0 {
		v = v[:plus]
	}
	core, pre := v, ""
	if dash := strings.IndexByte(v, '-'); dash >= 0 {
		core, pre = v[:dash], v[dash+1:]
	}
	parts := strings.Split(core, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			break
		}
		nums = append(nums, n)
	}
	return nums, pre
}

// dedupe drops results whose path was already seen, preserving order.
func dedupe(in []Result) []Result {
	seen := make(map[string]struct{}, len(in))
	out := make([]Result, 0, len(in))
	for _, r := range in {
		if _, ok := seen[r.Path]; ok {
			continue
		}
		seen[r.Path] = struct{}{}
		out = append(out, r)
	}
	return out
}
