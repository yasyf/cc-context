package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/toon"
)

// toonFlags holds the toon command's flags before they are mapped to
// toon.Options.
type toonFlags struct {
	delimiter string
	indent    int
	forceTOON bool
	strict    bool
	budget    int
}

func newToonCmd() *cobra.Command {
	var f toonFlags
	cmd := &cobra.Command{
		Use:   "toon [-- command [args...]]",
		Short: "Convert JSON/NDJSON to TOON, as a filter or by wrapping a command",
		Long: "Without `--`, read JSON or NDJSON on stdin and emit TOON (or compact JSON when " +
			"smaller). With `-- command …`, run the command, convert its JSON stdout in place, " +
			"stream its stderr, and exit with its code; non-JSON output passes through unchanged.",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := toonOptions(f)
			if err != nil {
				return err
			}

			dash := cmd.ArgsLenAtDash()
			if dash < 0 {
				return runToonFilter(cmd, opts, f.budget)
			}
			return runToonWrapper(cmd, args[dash:], opts, f.budget)
		},
	}
	cmd.Flags().StringVar(&f.delimiter, "delimiter", "comma", "array delimiter: comma|tab|pipe")
	cmd.Flags().IntVar(&f.indent, "indent", 2, "spaces per indentation level")
	cmd.Flags().BoolVar(&f.forceTOON, "force-toon", false, "always emit TOON, never compact JSON")
	cmd.Flags().BoolVar(&f.strict, "strict", false, "error on non-JSON input instead of passing it through")
	cmd.Flags().IntVar(&f.budget, "budget", 0, "token budget for the output")
	return cmd
}

// runToonFilter reads stdin and converts it.
func runToonFilter(cmd *cobra.Command, opts toon.Options, budget int) error {
	data, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	out, converted, err := toon.Convert(data, opts)
	if err != nil {
		return err
	}
	cmd.Print(terminate(render.Cap(out, budget), converted))
	return nil
}

// runToonWrapper runs argv, converts its stdout, and propagates its exit code as
// an *ExitError so main exits with the child's code.
func runToonWrapper(cmd *cobra.Command, argv []string, opts toon.Options, budget int) error {
	out, converted, code, err := toon.Run(cmd.Context(), argv, opts, cmd.InOrStdin(), os.Stderr)
	if err != nil {
		return err
	}
	cmd.Print(terminate(render.Cap(out, budget), converted))
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// terminate appends a trailing newline to converted output that lacks one so the
// table does not run into the next shell prompt; passthrough output (converted
// is false) is left byte-for-byte unchanged.
func terminate(out string, converted bool) string {
	if converted && out != "" && !strings.HasSuffix(out, "\n") {
		return out + "\n"
	}
	return out
}

// toonOptions maps the flags to toon.Options, validating the delimiter name.
func toonOptions(f toonFlags) (toon.Options, error) {
	delim, err := parseDelimiter(f.delimiter)
	if err != nil {
		return toon.Options{}, err
	}
	return toon.Options{
		Indent:    f.indent,
		Delimiter: delim,
		ForceTOON: f.forceTOON,
		Strict:    f.strict,
	}, nil
}

// parseDelimiter resolves a delimiter name to its TOON delimiter.
func parseDelimiter(name string) (toon.Delimiter, error) {
	switch name {
	case "comma":
		return toon.DelimiterComma, nil
	case "tab":
		return toon.DelimiterTab, nil
	case "pipe":
		return toon.DelimiterPipe, nil
	default:
		return 0, fmt.Errorf("invalid delimiter %q: want comma|tab|pipe", name)
	}
}
