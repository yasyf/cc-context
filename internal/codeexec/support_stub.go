//go:build !((darwin && arm64) || (linux && amd64) || (linux && arm64) || (windows && amd64))

package codeexec

// Supported reports whether the embedded monty runtime has a build for this
// platform.
const Supported = false

// UnsupportedReason explains why sandbox execution is unavailable; empty when
// Supported.
const UnsupportedReason = "monty ships no runtime for this platform (notably darwin/amd64 Intel Macs) — ccx exec is unavailable, everything else works"
