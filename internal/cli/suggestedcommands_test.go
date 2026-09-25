package cli_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/cli"
)

var reSuggestedCommand = regexp.MustCompile(`\bccx((?: +[a-z][a-z0-9-]*)+)`)

// TestSuggestedCommandsAreRegistered holds every "ccx …" a message hands the
// reader to the tree that message is printed by. A refusal naming a verb cobra
// does not carry sends them to an unknown-command error while already stuck.
func TestSuggestedCommandsAreRegistered(t *testing.T) {
	t.Parallel()
	root := cli.NewRootCmd()
	repo := repoRootFromCaller(t)
	checked := 0
	for _, src := range goSourcesUnder(t, repo) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, src, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		rel, err := filepath.Rel(repo, src)
		if err != nil {
			t.Fatalf("relativize %s: %v", src, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			text, ok := stringValue(n)
			if !ok {
				return true
			}
			for _, m := range reSuggestedCommand.FindAllStringSubmatch(text, -1) {
				args := strings.Fields(m[1])
				if subcommand(root, args[0]) == nil {
					// Prose naming only the binary — "ccx never minted %s".
					continue
				}
				checked++
				cmd, rest := descend(root, args)
				if len(rest) == 0 || !cmd.HasSubCommands() {
					continue
				}
				t.Errorf("%s:%d: message suggests %q, but %q has no %q subcommand — it carries %s",
					rel, fset.Position(n.Pos()).Line, "ccx "+strings.Join(args, " "),
					cmd.CommandPath(), rest[0], strings.Join(subcommandNames(cmd), ", "))
			}
			return false
		})
	}
	if checked == 0 {
		t.Fatal("scanned no ccx invocations; the literal scan is broken, not the tree clean")
	}
}

func stringValue(n ast.Node) (string, bool) {
	switch e := n.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		lhs, ok := stringValue(e.X)
		if !ok {
			return "", false
		}
		rhs, ok := stringValue(e.Y)
		if !ok {
			return "", false
		}
		return lhs + rhs, true
	}
	return "", false
}

func subcommand(cmd *cobra.Command, name string) *cobra.Command {
	for _, c := range cmd.Commands() {
		if c.Name() == name || slices.Contains(c.Aliases, name) {
			return c
		}
	}
	return nil
}

func descend(root *cobra.Command, args []string) (*cobra.Command, []string) {
	cmd := root
	for i, arg := range args {
		next := subcommand(cmd, arg)
		if next == nil {
			return cmd, args[i:]
		}
		cmd = next
	}
	return cmd, nil
}

func subcommandNames(cmd *cobra.Command) []string {
	names := make([]string, 0, len(cmd.Commands()))
	for _, c := range cmd.Commands() {
		names = append(names, c.Name())
	}
	return names
}

func goSourcesUnder(t *testing.T, root string) []string {
	t.Helper()
	var sources []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			sources = append(sources, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return sources
}

func repoRootFromCaller(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve this test's own path")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}
