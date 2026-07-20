package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/hunk"
)

// selectionPlan is the JSON contract between ship's jj lane and the
// apply-selection diff tool: the per-file selection and the sidecar path the
// tool writes a structured failure reason to.
type selectionPlan struct {
	Files  map[string]planFile `json:"files"`
	Result string              `json:"result"`
}

// planFile is one file's selection: skip or only, and the hunk refs by
// post-image range and content digest.
type planFile struct {
	Mode  string     `json:"mode"`
	Hunks []planHunk `json:"hunks"`
}

// planHunk pins a hunk by its post-image line range and content digest.
type planHunk struct {
	Range  string `json:"range"`
	Digest string `json:"digest"`
}

// buildSelectionPlan renders sel and the sidecar path into a plan the
// apply-selection tool consumes; every file inherits the ship's single mode.
func buildSelectionPlan(sel *shipSelection, resultPath string) selectionPlan {
	files := make(map[string]planFile, len(sel.files))
	for path, refs := range sel.files {
		hunks := make([]planHunk, len(refs))
		for i, ref := range refs {
			hunks[i] = planHunk{Range: refRange(ref), Digest: ref.Hash.String()}
		}
		files[path] = planFile{Mode: sel.mode.String(), Hunks: hunks}
	}
	return selectionPlan{Files: files, Result: resultPath}
}

// refRange renders a ref's post-image range as "A-B" (or "A" for a single line).
func refRange(ref anchor.Ref) string {
	if ref.End > ref.Line {
		return fmt.Sprintf("%d-%d", ref.Line, ref.End)
	}
	return strconv.Itoa(ref.Line)
}

// planRefs reconstructs anchor refs from a plan file's hunks so the tool resolves
// them exactly as the pre-flight pass did.
func planRefs(hunks []planHunk) ([]anchor.Ref, error) {
	refs := make([]anchor.Ref, len(hunks))
	for i, h := range hunks {
		start, end, ok, err := anchor.ParseNumericRange(h.Range)
		if !ok || err != nil {
			return nil, fmt.Errorf("invalid plan hunk range %q", h.Range)
		}
		ref := anchor.Ref{Line: start, Hash: anchor.Hash(h.Digest)}
		if end != start {
			ref.End = end
		}
		refs[i] = ref
	}
	return refs, nil
}

// jjSelectArgv builds the jj commit/squash argv that drives the apply-selection
// diff tool: the base verb, the three --config flags wiring the merge tool, the
// --tool selector, the message flags, then the scoped paths.
func jjSelectArgv(o shipOpts, planPath string) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("ship: resolve executable: %w", err)
	}
	argv := make([]string, 0, 16+len(o.paths))
	if o.amend {
		argv = append(argv, "squash")
	} else {
		argv = append(argv, "commit")
	}
	argv = append(argv,
		"--config", "merge-tools.ccx-ship-select.program="+tomlQuote(exe),
		"--config", "merge-tools.ccx-ship-select.edit-args="+tomlArray("vcs", "apply-selection", "--plan", planPath, "$left", "$right"),
		"--config", "ui.diff-instructions=false",
		"--tool", "ccx-ship-select",
	)
	switch {
	case o.amend && o.message != "":
		argv = append(argv, "-m", o.message)
	case o.amend:
		argv = append(argv, "--use-destination-message")
	default:
		argv = append(argv, "-m", o.message)
	}
	if len(o.paths) > 0 {
		argv = append(argv, "--")
		argv = append(argv, o.paths...)
	}
	return argv, nil
}

// tomlQuote renders s as a TOML basic string so a program path with spaces
// survives jj's --config TOML parse.
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// tomlArray renders elems as a single-line TOML array of basic strings.
func tomlArray(elems ...string) string {
	quoted := make([]string, len(elems))
	for i, e := range elems {
		quoted[i] = tomlQuote(e)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

// readSidecar returns the structured failure reason the apply-selection tool
// wrote, or "" when the tool never got that far.
func readSidecar(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // the sidecar path is ship's own tempfile, not untrusted input
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// newApplySelectionCmd builds the hidden diff-editor tool jj invokes per plan:
// it rewrites each planned file in the right tree to its selected hunks and
// leaves the read-only left tree untouched.
func newApplySelectionCmd() *cobra.Command {
	var planPath string
	cmd := &cobra.Command{
		Use:    "apply-selection --plan <file> <left> <right>",
		Short:  "Internal jj diff tool: rewrite the right tree to a hunk selection",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return runApplySelection(planPath, args[0], args[1])
		},
	}
	cmd.Flags().StringVar(&planPath, "plan", "", "path to the selection plan JSON")
	_ = cmd.MarkFlagRequired("plan")
	return cmd
}

// runApplySelection applies every planned file's selection to the right tree,
// exiting nonzero (so jj aborts atomically) on the first failure and recording
// the reason to the plan's sidecar for ship to surface.
func runApplySelection(planPath, leftDir, rightDir string) error {
	plan, err := loadSelectionPlan(planPath)
	if err != nil {
		return err
	}
	for path, pf := range plan.Files {
		if err := applySelectionFile(leftDir, rightDir, path, pf); err != nil {
			writeSidecar(plan.Result, err.Error())
			return err
		}
	}
	return nil
}

// loadSelectionPlan reads and decodes the plan ship wrote.
func loadSelectionPlan(path string) (selectionPlan, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is ship's own tempfile, passed by jj
	if err != nil {
		return selectionPlan{}, fmt.Errorf("read selection plan: %w", err)
	}
	var plan selectionPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return selectionPlan{}, fmt.Errorf("parse selection plan %q: %w", path, err)
	}
	return plan, nil
}

// applySelectionFile resolves path's refs against jj's own left (@-) and right
// (@) snapshots and rewrites the right file to the selected hunks, preserving its
// mode. left is read-only; a file absent from left is a new file (empty base).
func applySelectionFile(leftDir, rightDir, path string, pf planFile) error {
	base, err := readTreeFile(leftDir, path)
	if err != nil {
		return err
	}
	rightPath := filepath.Join(rightDir, path)
	current, err := os.ReadFile(rightPath) //nolint:gosec // rightPath is jj's snapshot dir, not user input
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	refs, err := planRefs(pf.Hunks)
	if err != nil {
		return err
	}
	mode := selectSkip
	if pf.Mode == selectOnly.String() {
		mode = selectOnly
	}
	hunks, keep, err := resolveFileKeep(path, base, current, refs, mode)
	if err != nil {
		return err
	}
	selected := hunk.Select(base, hunks, keep)
	info, err := os.Stat(rightPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.WriteFile(rightPath, selected, info.Mode().Perm()); err != nil { //nolint:gosec // preserving the snapshot file's existing mode
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// readTreeFile reads dir/path, treating a missing file as an empty base (a file
// added since the parent commit); any other read error is fatal.
func readTreeFile(dir, path string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, path)) //nolint:gosec // dir is jj's snapshot dir, not user input
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read base %s: %w", path, err)
	}
	return data, nil
}

// writeSidecar records reason for ship to read; a write failure is non-fatal
// because the nonzero exit and stderr already signal the abort.
func writeSidecar(path, reason string) {
	_ = os.WriteFile(path, []byte(reason+"\n"), 0o600) //nolint:gosec,errcheck // best-effort structured reason; the nonzero exit is the primary signal
}
