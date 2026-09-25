package codeexec

import (
	"context"

	"github.com/yasyf/cc-context/internal/render"
)

// Supported reports whether sandbox execution is available: the driver needs
// uv on the PATH ctx carries for its children to provision its Python runtime.
func Supported(ctx context.Context) bool { return render.LookPath(ctx, "uv") != "" }

// UnsupportedReason explains the missing prerequisite when Supported is false.
const UnsupportedReason = "ccx exec needs uv on PATH to run its Python sandbox (brew install uv) — everything else works"
