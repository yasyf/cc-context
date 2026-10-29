package read

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/outline"
)

func TestNumericWindowParity(t *testing.T) {
	contents := []string{
		"", "a", "a\n", "\n", "\n\n", "a\n\n", "a\r\nb\r\nlast\r",
		"a\n\xff\xfe\x00\nlast", strings.Repeat("x", 4096), strings.Repeat("x", 4096) + "\n",
		strings.Repeat("x", 256<<10) + "\nselected\n" + strings.Repeat("z", 256<<10),
	}
	for i, content := range contents {
		for _, bounds := range [][2]int{{1, 1}, {1, 10}, {2, 2}, {2, 3}, {3, 100}, {0, 1}, {100, 200}} {
			t.Run(fmt.Sprintf("%d/%d-%d", i, bounds[0], bounds[1]), func(t *testing.T) {
				wantAll := anchor.FromBytes("", []byte(content)).Lines()
				var want []string
				for line, text := range wantAll {
					if line+1 >= bounds[0] && line+1 <= bounds[1] && bounds[0] > 0 {
						want = append(want, text)
					}
				}
				readers := []io.Reader{strings.NewReader(content), iotest.HalfReader(strings.NewReader(content))}
				for _, reader := range readers {
					got, total, err := numericWindow(reader, bounds[0], bounds[1])
					if err != nil || total != len(wantAll) || !reflect.DeepEqual(got, want) {
						t.Fatalf("numericWindow = %q, %d, %v; want %q, %d, nil", got, total, err, want, len(wantAll))
					}
				}
			})
		}
	}
}

func TestRunNumericParity(t *testing.T) {
	contents := []string{"", "alpha", "alpha\n", "\n", "\n\n", "alpha\r\nbeta\r\nlast\r", "alpha\nbeta\n\n", "text" + strings.Repeat(" ", 4096) + "\n\xff\xfe\nlast", strings.Repeat("x", 256<<10) + "\nselected\nlast"}
	sections := []string{"1", "2", "1-3", "2-999", "999", "0", "0-2", "3-1", "01-02", " 1 , 2 ", "2,1", "999999999999999999999999", "1--2", "-1", "## Heading", ""}
	for i, content := range contents {
		path := write(t, "window.md", content)
		for _, section := range sections {
			for _, full := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/%s/%t", i, section, full), func(t *testing.T) {
					a := backend.Args{Path: path, Section: section, Full: full}
					want, wantErr := referenceRead(a)
					got, err := Run(a)
					if got != want || fmt.Sprint(err) != fmt.Sprint(wantErr) {
						t.Fatalf("Run = %q, %v; want %q, %v", got, err, want, wantErr)
					}
				})
			}
		}
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "missing"), t.TempDir(), write(t, "binary", "a\x00b")} {
		for _, section := range []string{"1", "0", "3-1"} {
			a := backend.Args{Path: path, Section: section}
			want, wantErr := referenceRead(a)
			got, err := Run(a)
			if got != want || fmt.Sprint(err) != fmt.Sprint(wantErr) {
				t.Fatalf("Run(%q, %q) = %q, %v; want %q, %v", path, section, got, err, want, wantErr)
			}
		}
	}
}

func referenceRead(a backend.Args) (string, error) {
	if skip, ok := outline.BinarySkip(a.Path); ok {
		return skip, nil
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", a.Path, err)
	}
	lines := anchor.FromBytes(a.Path, data).Lines()
	if len(lines) == 0 {
		return fmt.Sprintf("# read %s: empty file\n", a.Path), nil
	}
	whole := a.Full || a.Section == ""
	start, end := 1, len(lines)
	if !whole {
		var numeric bool
		start, end, numeric, err = anchor.ParseNumericRange(a.Section)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", a.Path, err)
		}
		if numeric {
			if start < 1 {
				return "", fmt.Errorf("read %s --section %s: line %d is invalid — lines are 1-indexed", a.Path, a.Section, start)
			}
			if start > len(lines) {
				return "", fmt.Errorf("read %s --section %s: starts past EOF (file has %d lines)", a.Path, a.Section, len(lines))
			}
			if end > len(lines) {
				end = len(lines)
			}
		} else {
			start, end, err = resolveSection(a.Section, a.Path, lines)
			if err != nil {
				return "", err
			}
		}
	}
	served := lines[start-1 : end]
	span := anchor.FormatRange(start, end, anchor.Of(served[0]))
	if start == end {
		span = anchor.Format(start, anchor.Of(served[0]))
	}
	count := fmt.Sprintf("(%d of %d lines)", end-start+1, len(lines))
	if whole {
		count = fmt.Sprintf("(%d lines)", len(lines))
	}
	return fmt.Sprintf("# read %s:%s %s\n%s\n", a.Path, span, count, strings.Join(served, "\n")), nil
}

func TestNumericWindowReadsThroughSelectedWindow(t *testing.T) {
	wantErr := errors.New("read failed after window")
	for _, bounds := range [][2]int{{1, 1}, {0, 0}, {99, 100}} {
		r := io.MultiReader(strings.NewReader("selected\n"+strings.Repeat("x", 16<<10)), iotest.ErrReader(wantErr))
		_, _, err := numericWindow(r, bounds[0], bounds[1])
		if !errors.Is(err, wantErr) {
			t.Fatalf("numericWindow(%v) error = %v; want %v", bounds, err, wantErr)
		}
	}
}

func TestNumericWindowAllocationBound(t *testing.T) {
	for _, content := range []string{
		"selected\n" + strings.Repeat("unselected\n", 400_000),
		strings.Repeat("x", 4<<20) + "\nselected\n" + strings.Repeat("z", 4<<20),
	} {
		start := 1
		if content[0] == 'x' {
			start = 2
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got, _, err := numericWindow(strings.NewReader(content), start, start)
		runtime.ReadMemStats(&after)
		if err != nil || len(got) != 1 || got[0] != "selected" {
			t.Fatalf("numericWindow = %q, %v", got, err)
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 512<<10 {
			t.Fatalf("allocated %d bytes for %d input bytes and 8 selected bytes; limit %d", allocated, len(content), 512<<10)
		}
	}
}
