package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/codeexec"
	"github.com/yasyf/cc-context/internal/proxy"
)

var errNoScript = errors.New("no script: pass one as an argument, with --file, or on stdin (or use --list-tools)")

func newExecCmd() *cobra.Command {
	var (
		file      string
		budget    int
		listTools bool
	)
	cmd := &cobra.Command{
		Use:   "exec [script]",
		Short: "Compose ccx ops, sh, and reflected MCP tools in a sandboxed Python script, returning only the distilled result",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !codeexec.Supported {
				return errors.New(codeexec.UnsupportedReason)
			}
			var script string
			if !listTools {
				var err error
				script, err = execScript(cmd, args, file)
				if err != nil {
					return err
				}
			}
			store, err := codeexec.NewDiskStore()
			if err != nil {
				return err
			}
			p := proxy.New()
			defer func() { _ = p.Close() }()
			eng := codeexec.NewEngine(p, store)
			defer func() { _ = eng.Close() }()

			var out string
			var notes []string
			if listTools {
				out, notes, err = eng.Tools(cmd.Context())
			} else {
				out, notes, err = eng.Exec(cmd.Context(), script, budget)
			}
			for _, n := range notes {
				cmd.PrintErrln("note:", n)
			}
			if err != nil {
				return err
			}
			cmd.Print(out)
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", `read the script from this file ("-" means stdin)`)
	cmd.Flags().IntVar(&budget, "budget", 0, "token budget for the result")
	cmd.Flags().BoolVar(&listTools, "list-tools", false, "print the sandbox preamble and every available tool signature")
	return cmd
}

// execScript resolves the script source in precedence order: the positional
// arg, then --file ("-" means stdin), then piped stdin.
func execScript(cmd *cobra.Command, args []string, file string) (string, error) {
	switch {
	case len(args) == 1:
		return args[0], nil
	case file == "-":
		return readScript(cmd.InOrStdin())
	case file != "":
		f, err := os.Open(file) //nolint:gosec // --file names the script to run by design
		if err != nil {
			return "", fmt.Errorf("open script: %w", err)
		}
		defer func() { _ = f.Close() }()
		return readScript(f)
	case stdinPiped(cmd):
		return readScript(cmd.InOrStdin())
	}
	return "", errNoScript
}

// readScript reads r to the end; an all-whitespace script counts as missing so
// an empty pipe fails as a usage error, not a sandbox parse error.
func readScript(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("read script: %w", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", errNoScript
	}
	return string(data), nil
}

// stdinPiped reports whether the command's input carries data rather than an
// interactive terminal: a replaced in-stream (tests, wrappers) always counts,
// a real *os.File counts unless it is a character device.
func stdinPiped(cmd *cobra.Command) bool {
	f, ok := cmd.InOrStdin().(*os.File)
	if !ok {
		return true
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}
