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
