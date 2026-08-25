package vcstest

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Quiesce blocks until log has stopped growing for 300ms, so records from a
// detached child a tool left running past its own exit — gt state's cache
// refresher spawns four more git processes after gt returns — are all in
// before the caller reads them.
func Quiesce(t *testing.T, log string) {
	t.Helper()
	if !waitQuiet(log) {
		t.Fatalf("argv log %s still growing after 5s", log)
	}
}

// waitQuiet reports whether log's size held still for six 50ms polls within a
// 5s deadline; a missing log counts as still.
func waitQuiet(log string) bool {
	deadline := time.Now().Add(5 * time.Second)
	last, stable := int64(-1), 0
	for stable < 6 {
		if time.Now().After(deadline) {
			return false
		}
		var size int64
		if info, err := os.Stat(log); err == nil {
			size = info.Size()
		}
		if size == last {
			stable++
		} else {
			stable = 0
		}
		last = size
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// Invocation is one recorded tool call: the argv, and the working directory it
// ran in.
type Invocation struct {
	Dir  string
	Argv []string
}

// Invocations returns the depth-0 argv records in log — the tool calls ccx
// itself made, without the children those tools spawned. A log never written
// reads as no invocations.
func Invocations(t *testing.T, log string) [][]string {
	t.Helper()
	return InvocationsAtDepth(t, log, 0)
}

// InvocationsAtDepth returns log's argv records at the given spawn depth:
// depth 1 holds the children a depth-0 tool spawned, and so on.
func InvocationsAtDepth(t *testing.T, log string, depth int) [][]string {
	t.Helper()
	records := RecordsAtDepth(t, log, depth)
	if len(records) == 0 {
		return nil
	}
	out := make([][]string, 0, len(records))
	for _, r := range records {
		out = append(out, r.Argv)
	}
	return out
}

// Records returns the depth-0 invocations in log, each with the directory the
// tool ran in.
func Records(t *testing.T, log string) []Invocation {
	t.Helper()
	return RecordsAtDepth(t, log, 0)
}

// RecordsAtDepth is Records at the given spawn depth.
func RecordsAtDepth(t *testing.T, log string, depth int) []Invocation {
	t.Helper()
	data, err := os.ReadFile(log) //nolint:gosec // log is the shim's own path, minted under the test's TempDir
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read argv log: %v", err)
	}
	if len(data) == 0 {
		return nil
	}
	if data[len(data)-1] != 0 {
		t.Fatalf("argv log %s: final record missing its trailing NUL", log)
	}
	fields := strings.Split(string(data[:len(data)-1]), "\x00")
	var out []Invocation
	for i := 0; i < len(fields); {
		if len(fields)-i < 3 {
			t.Fatalf("argv log %s: dangling record header at field %d", log, i)
		}
		d, err := strconv.Atoi(fields[i])
		if err != nil {
			t.Fatalf("argv log %s: depth %q at field %d: %v", log, fields[i], i, err)
		}
		dir := fields[i+1]
		argc, err := strconv.Atoi(fields[i+2])
		if err != nil {
			t.Fatalf("argv log %s: argc %q at field %d: %v", log, fields[i+2], i+2, err)
		}
		if argc < 1 || i+3+argc > len(fields) {
			t.Fatalf("argv log %s: argc %d at field %d overruns the log", log, argc, i+2)
		}
		if d == depth {
			out = append(out, Invocation{Dir: dir, Argv: slices.Clone(fields[i+3 : i+3+argc])})
		}
		i += 3 + argc
	}
	return out
}
