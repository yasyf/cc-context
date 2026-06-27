// Command ccx: Compact codebase-context tools for AI agents
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/yasyf/cc-context/internal/cli"
	applog "github.com/yasyf/cc-context/internal/log"
)

func main() {
	applog.Setup()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := cli.NewRootCmd().ExecuteContext(ctx)
	if err == nil {
		return
	}
	// A bare *ExitError only carries the child's exit code; the toon wrapper has
	// already mirrored the child's stderr, so stay silent and just exit with it.
	var ee *cli.ExitError
	if !errors.As(err, &ee) {
		fmt.Fprintln(os.Stderr, "ccx:", err)
	}
	os.Exit(cli.ExitCode(err))
}
