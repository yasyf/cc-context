package cli

import (
	"reflect"
	"testing"

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
		name        string
		rate, quiet string
		want        cleanup.BusyDeletes
		wantErr     bool
	}{
		{name: "unset"},
		{name: "rate and cleaned roots", rate: "500\n", quiet: "/a/b/\n/c\n", want: cleanup.BusyDeletes{Rate: 500, Quiet: []string{"/a/b", "/c"}}},
		{name: "zero rate", rate: "0", wantErr: true},
		{name: "not a number", rate: "fast", wantErr: true},
		{name: "relative root", rate: "500", quiet: "a/b", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCleanupBusyDeletes(tt.rate, tt.quiet)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseCleanupBusyDeletes(%q, %q) error = %v, wantErr %t", tt.rate, tt.quiet, err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseCleanupBusyDeletes(%q, %q) = %+v, want %+v", tt.rate, tt.quiet, got, tt.want)
			}
		})
	}
}
