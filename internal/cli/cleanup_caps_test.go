package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestParseCleanupRateCaps(t *testing.T) {
	tests := []struct {
		name    string
		values  string
		want    []cleanup.RateCap
		wantErr bool
	}{
		{name: "unset", values: ""},
		{name: "one", values: "/a/b=100\n", want: []cleanup.RateCap{{Root: "/a/b", Rate: 100}}},
		{name: "several, cleaned", values: "/a/b/=100\n/c=5\n", want: []cleanup.RateCap{{Root: "/a/b", Rate: 100}, {Root: "/c", Rate: 5}}},
		{name: "a root holding an equals sign", values: "/a=b=7", want: []cleanup.RateCap{{Root: "/a=b", Rate: 7}}},
		{name: "no rate", values: "/a/b", wantErr: true},
		{name: "relative root", values: "a/b=100", wantErr: true},
		{name: "zero rate", values: "/a/b=0", wantErr: true},
		{name: "not a number", values: "/a/b=fast", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCleanupRateCaps(tt.values)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseCleanupRateCaps(%q) error = %v, wantErr %t", tt.values, err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseCleanupRateCaps(%q) = %+v, want %+v", tt.values, got, tt.want)
			}
		})
	}
}

func TestParseCleanupBusyDeletes(t *testing.T) {
	tests := []struct {
		name                 string
		rate, workers, quiet string
		want                 cleanup.BusyDeletes
		wantErr              bool
	}{
		{name: "unset", want: cleanup.BusyDeletes{Workers: 1, MoveEvery: cleanupMoveEveryDefault}},
		{name: "rate and cleaned roots", rate: "500\n", quiet: "/a/b/\n/c\n", want: cleanup.BusyDeletes{Rate: 500, Workers: 1, Quiet: []string{"/a/b", "/c"}, MoveEvery: cleanupMoveEveryDefault}},
		{name: "workers", rate: "500", workers: "4\n", want: cleanup.BusyDeletes{Rate: 500, Workers: 4, MoveEvery: cleanupMoveEveryDefault}},
		{name: "zero workers", workers: "0", wantErr: true},
		{name: "zero rate", rate: "0", wantErr: true},
		{name: "not a number", rate: "fast", wantErr: true},
		{name: "relative root", rate: "500", quiet: "a/b", wantErr: true},
		{name: "a root holding a space", rate: "500", quiet: "/Volumes/Quiet Disk\n", want: cleanup.BusyDeletes{Rate: 500, Workers: 1, Quiet: []string{"/Volumes/Quiet Disk"}, MoveEvery: cleanupMoveEveryDefault}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCleanupBusyDeletes(tt.rate, tt.workers, tt.quiet, "", "")
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseCleanupBusyDeletes(%q, %q) error = %v, wantErr %t", tt.rate, tt.quiet, err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseCleanupBusyDeletes(%q, %q) = %+v, want %+v", tt.rate, tt.quiet, got, tt.want)
			}
		})
	}
}

func TestCleanupRootsResolveSymlinks(t *testing.T) {
	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	busy, err := parseCleanupBusyDeletes("500", "", link, link, "1m")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{target}; !reflect.DeepEqual(busy.Quiet, want) || !reflect.DeepEqual(busy.Move, want) {
		t.Errorf("quiet roots = %v, move roots = %v, want %v", busy.Quiet, busy.Move, want)
	}
	if busy.MoveEvery != time.Minute {
		t.Errorf("move every = %s, want 1m", busy.MoveEvery)
	}
	caps, err := parseCleanupRateCaps(link + "=100")
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) != 1 || caps[0].Root != target {
		t.Errorf("caps = %+v, want root %s", caps, target)
	}
}
