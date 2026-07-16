// Package lookpath resolves executables on PATH behind a single stubbable seam.
package lookpath

import "os/exec"

// Find returns the path to the named executable on PATH, or "" when it is
// absent. It is a package var so tests can stub PATH resolution.
var Find = func(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}
