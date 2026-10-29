package read

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
)

func TestRunNumericAllocationBound(t *testing.T) {
	path := write(t, "window.txt", strings.Repeat("x", 4<<20)+"\nselected\n"+strings.Repeat("z", 4<<20))
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := Run(backend.Args{Path: path, Section: "2"})
	runtime.ReadMemStats(&after)
	want := fmt.Sprintf("# read %s:%s (1 of 3 lines)\nselected\n", path, anchor.Format(2, anchor.Of("selected")))
	if err != nil || got != want {
		t.Fatalf("Run = %q, %v; want %q, nil", got, err, want)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 512<<10 {
		t.Fatalf("numeric Run allocated %d bytes for 8 MiB of unselected text; limit %d", allocated, 512<<10)
	}
}
