package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/format"
	"github.com/yasyf/cc-context/internal/render"
)

// toonFlags holds the toon command's flags before they are mapped to
// format.Options.
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
func runToonFilter(cmd *cobra.Command, opts format.Options, budget int) error {
	data, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	out, converted, err := format.Convert(data, opts)
	if err != nil {
		return err
	}
	cmd.Print(terminate(render.Cap(out, budget), converted))
	return nil
}

// runToonWrapper runs argv, converts its stdout, and propagates its exit code as
// an *ExitError so main exits with the child's code.
func runToonWrapper(cmd *cobra.Command, argv []string, opts format.Options, budget int) error {
	out, converted, code, err := format.Run(cmd.Context(), argv, opts, cmd.InOrStdin(), os.Stderr)
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

// toonOptions maps the flags to format.Options, validating the delimiter name.
func toonOptions(f toonFlags) (format.Options, error) {
	delim, err := parseDelimiter(f.delimiter)
	if err != nil {
		return format.Options{}, err
	}
	opts := format.Options{
		Format:    format.FormatAuto,
		Indent:    f.indent,
		Delimiter: delim,
		Strict:    f.strict,
	}
	if f.forceTOON {
		opts.Format = format.FormatTOON
	}
	return opts, nil
}

// parseDelimiter resolves a delimiter name to its TOON delimiter.
func parseDelimiter(name string) (format.Delimiter, error) {
	switch name {
	case "comma":
		return format.DelimiterComma, nil
	case "tab":
		return format.DelimiterTab, nil
	case "pipe":
		return format.DelimiterPipe, nil
	default:
		return 0, fmt.Errorf("invalid delimiter %q: want comma|tab|pipe", name)
	}
}
