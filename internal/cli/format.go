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

// formatFlags holds the format command's flags before they are mapped to
// format.Options.
type formatFlags struct {
	format    string
	delimiter string
	indent    int
	strict    bool
	budget    int
}

func newFormatCmd() *cobra.Command {
	var f formatFlags
	cmd := &cobra.Command{
		Use:   "format [-- command [args...]]",
		Short: "Re-encode JSON/NDJSON token-lean, as a filter or by wrapping a command",
		Long: "Without `--`, read JSON or NDJSON on stdin and emit the leanest encoding for its " +
			"shape — prose, markdown, CSV/TSV, TOON, TRON, JSONL, or compact JSON — never larger " +
			"than compact JSON. With `-- command …`, run the command, convert its JSON stdout in " +
			"place, stream its stderr, and exit with its code; non-JSON output passes through " +
			"unchanged. --format=X forces one encoder, even when larger.",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := formatOptions(f)
			if err != nil {
				return err
			}

			dash := cmd.ArgsLenAtDash()
			if dash < 0 {
				return runFormatFilter(cmd, opts, f.budget)
			}
			return runFormatWrapper(cmd, args[dash:], opts, f.budget)
		},
	}
	cmd.Flags().StringVar(&f.format, "format", "auto", "output format: auto|toon|tron|csv|tsv|markdown|jsonl|prose|json")
	cmd.Flags().StringVar(&f.delimiter, "delimiter", "comma", "array delimiter for TOON output only: comma|tab|pipe")
	cmd.Flags().IntVar(&f.indent, "indent", 2, "spaces per indentation level, TOON output only")
	cmd.Flags().BoolVar(&f.strict, "strict", false, "error on non-JSON input instead of passing it through")
	cmd.Flags().IntVar(&f.budget, "budget", 0, "token budget for the output")
	return cmd
}

// runFormatFilter reads stdin and converts it.
func runFormatFilter(cmd *cobra.Command, opts format.Options, budget int) error {
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

// runFormatWrapper runs argv, converts its stdout, and propagates its exit code
// as an *ExitError so main exits with the child's code.
func runFormatWrapper(cmd *cobra.Command, argv []string, opts format.Options, budget int) error {
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

// formatOptions maps the flags to format.Options, validating the format and
// delimiter names.
func formatOptions(f formatFlags) (format.Options, error) {
	fm, err := format.ParseFormat(f.format)
	if err != nil {
		return format.Options{}, err
	}
	delim, err := parseDelimiter(f.delimiter)
	if err != nil {
		return format.Options{}, err
	}
	return format.Options{
		Format:    fm,
		Indent:    f.indent,
		Delimiter: delim,
		Strict:    f.strict,
	}, nil
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
