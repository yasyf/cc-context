package codeexec

import "github.com/yasyf/cc-context/internal/lookpath"

// Supported reports whether sandbox execution is available: the driver needs
// uv on PATH to provision its Python runtime.
func Supported() bool { return lookpath.Find("uv") != "" }

// UnsupportedReason explains the missing prerequisite when Supported is false.
const UnsupportedReason = "ccx exec needs uv on PATH to run its Python sandbox (brew install uv) — everything else works"
