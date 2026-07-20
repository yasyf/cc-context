package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/anchor"
)

func newAnchorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "anchor",
		Short: "Content-anchor helpers: hash a line, resolve a ref",
		Args:  cobra.NoArgs,
		RunE:  groupHelp,
	}
	cmd.AddCommand(
		newAnchorHashCmd(),
		newAnchorResolveCmd(),
	)
	return cmd
}

func newAnchorHashCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "hash [text]",
		Short: "Print the 4-char content anchor hash of a line (arg, or stdin when \"-\" or absent)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			line := ""
			if len(args) == 1 && args[0] != "-" {
				line = args[0]
			} else {
				data, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("read line from stdin: %w", err)
				}
				line = strings.TrimRight(string(data), "\n")
			}
			cmd.Println(anchor.Of(line))
			return nil
		},
	}
}

func newAnchorResolveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resolve <file> <ref>",
		Short: "Resolve an anchor ref (A#hash, A-B#hash, or bare hash) to its line range in <file>",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, ok, err := anchor.Parse(args[1])
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("not an anchor ref %q: want \"A#hash\", \"A-B#hash\", or a bare \"hash\"", args[1])
			}
			f, err := anchor.Load(args[0])
			if err != nil {
				return err
			}
			rng, move, err := f.Resolve(ref)
			if err != nil {
				return err
			}
			cmd.Print(anchor.FormatRange(rng.Start, rng.End, ref.Hash) + "\n" + anchor.MoveNote(ref.Hash, move))
			return nil
		},
	}
}
