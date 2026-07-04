//go:build (darwin && arm64) || (linux && amd64) || (linux && arm64) || (windows && amd64)

package codeexec

// Supported reports whether the embedded monty runtime has a build for this
// platform.
const Supported = true

// UnsupportedReason explains why sandbox execution is unavailable; empty when
// Supported.
const UnsupportedReason = ""
