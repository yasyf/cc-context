package render

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/yasyf/cc-context/internal/vcs"
)

// diffFileHeader matches a tilth structural-diff per-file header, capturing the working-side path and symbol count.
var diffFileHeader = regexp.MustCompile(`^## c/\S+ w/(\S+) \((\d+) symbols\)$`)

// RunDiffCLI runs the tilth diff, supplements any empty-hunk section with its raw jj-aware hunk, and caps to budget.
func RunDiffCLI(ctx context.Context, bin string, argv []string, source string, budget int) (string, error) {
	out, err := RunCLI(ctx, bin, argv)
	if err != nil {
		return "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	fetch := func(workPath string) (string, error) {
		hunkArgv, herr := vcs.RawHunkArgv(ctx, cwd, source, workPath)
		if herr != nil {
			return "", herr
		}
		return RunCLI(ctx, hunkArgv[0], hunkArgv[1:])
	}
	supplemented, err := SupplementDiff(out, fetch)
	if err != nil {
		return "", err
	}
	return Cap(supplemented, budget), nil
}

// SupplementDiff appends a raw textual hunk (via fetch) to each empty "(0 symbols)" file section in tilth's diff output.
func SupplementDiff(out string, fetch func(workPath string) (string, error)) (string, error) {
	lines := strings.Split(out, "\n")
	var b strings.Builder
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		b.WriteString(line)
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}

		m := diffFileHeader.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[2] != "0" || strings.TrimSpace(sectionBody(lines, i+1)) != "" {
			continue
		}

		hunk, err := fetch(m[1])
		if err != nil {
			return "", fmt.Errorf("supplement diff for %q: %w", m[1], err)
		}
		hunk = strings.TrimRight(hunk, "\n")
		if hunk == "" {
			continue
		}
		b.WriteString("\n")
		b.WriteString(hunk)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// sectionBody returns the lines from start up to (but not including) the next
// "## " file header or EOF, joined as the section body.
func sectionBody(lines []string, start int) string {
	next := start
	for next < len(lines) && !strings.HasPrefix(lines[next], "## ") {
		next++
	}
	return strings.Join(lines[start:next], "\n")
}
