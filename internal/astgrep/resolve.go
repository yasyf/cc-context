package astgrep

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/yasyf/cc-context/internal/lookpath"
)

// minVersion is the ast-grep floor the structural ops require. Below it, argv
// flags and outline JSON shapes drift from what the renderers parse.
var minVersion = version{0, 44, 0}

// versionChecks memoizes binary paths that passed the floor probe; failures
// re-probe so a binary fixed in place recovers without a proxy restart.
var versionChecks sync.Map

// resolveBin returns the ast-grep binary to exec: the configured path when set,
// else the one on PATH (only "ast-grep" — never "sg", which is shadow-utils
// setgroups on Linux). It fails when ast-grep is absent or below the version floor.
func resolveBin(configured string) (string, error) {
	bin := configured
	if bin == "" {
		bin = lookpath.Find("ast-grep")
	}
	if bin == "" {
		return "", fmt.Errorf("ccx structural search, replace, and outline need ast-grep on PATH; install: brew install ast-grep (or: uv tool install ast-grep-cli)")
	}
	if err := checkVersion(bin); err != nil {
		return "", err
	}
	return bin, nil
}

// checkVersion runs the floor probe for bin, memoizing only a pass.
func checkVersion(bin string) error {
	if _, ok := versionChecks.Load(bin); ok {
		return nil
	}
	if err := probeVersion(bin); err != nil {
		return err
	}
	versionChecks.Store(bin, struct{}{})
	return nil
}

// probeVersion execs `<bin> --version` and enforces the floor, erroring on an
// unparseable line (named with the binary path and raw output) or a version below
// minVersion.
func probeVersion(bin string) error {
	out, err := exec.Command(bin, "--version").Output() //nolint:gosec // bin is a resolved ast-grep path
	if err != nil {
		return fmt.Errorf("ast-grep: run %q --version: %w", bin, err)
	}
	v, ok := parseVersion(string(out))
	if !ok {
		return fmt.Errorf("ast-grep: unparseable --version output from %q: %q", bin, strings.TrimSpace(string(out)))
	}
	if v.less(minVersion) {
		return fmt.Errorf("ccx needs ast-grep >= %s (found %s); upgrade: brew upgrade ast-grep", minVersion, v)
	}
	return nil
}

// version is a parsed major.minor.patch triple.
type version [3]int

// String renders the triple as "X.Y.Z".
func (v version) String() string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// less reports whether v orders before o.
func (v version) less(o version) bool {
	for i := range v {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return false
}

// parseVersion pulls the first X.Y.Z token out of an `ast-grep --version` line
// ("ast-grep 0.44.0"), trimming any prerelease or build suffix ("0.44.0-beta.1").
func parseVersion(out string) (version, bool) {
	for _, field := range strings.Fields(out) {
		core := field
		if i := strings.IndexAny(core, "-+"); i >= 0 {
			core = core[:i]
		}
		parts := strings.Split(core, ".")
		if len(parts) != 3 {
			continue
		}
		var v version
		ok := true
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				ok = false
				break
			}
			v[i] = n
		}
		if ok {
			return v, true
		}
	}
	return version{}, false
}
