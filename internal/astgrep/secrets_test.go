package astgrep

import (
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/anchor"
)

const rawAWSKey = "AKIAIOSFODNN7EXAMPLE" //nolint:gosec // AWS's documented example key id, not a credential

// TestRenderOutlineMasksSecrets proves the structural outline masks each file's
// section in that file's path context — a secret embedded in an item signature
// comes out masked with its rule id reported — while reveal passes signatures
// through raw and a secret-free outline is byte-identical with no ids.
func TestRenderOutlineMasksSecrets(t *testing.T) {
	secretItem := mkItem("key", 0, 0)
	secretItem.Signature = "key = \"" + rawAWSKey + "\""
	plainItem := mkItem("count", 0, 0)
	plainItem.Signature = "count = 10"

	tests := []struct {
		name    string
		item    OutlineItem
		reveal  bool
		want    string
		wantIDs []string
	}{
		{
			name:    "secret signature masked",
			item:    secretItem,
			want:    "# cfg.go\nL1  key = \"AKIA…[masked:aws-access-token]\"\n",
			wantIDs: []string{"aws-access-token"},
		},
		{
			name:   "reveal passes the signature raw",
			item:   secretItem,
			reveal: true,
			want:   "# cfg.go\nL1  key = \"" + rawAWSKey + "\"\n",
		},
		{
			name: "no findings byte-identical",
			item: plainItem,
			want: "# cfg.go\nL1  count = 10\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := []OutlineFile{{Path: "cfg.go", Items: []OutlineItem{tt.item}}}
			got, ids := RenderOutline(files, anchor.NewFiles(t.TempDir()), maxOutlineDepth, tt.reveal)
			if got != tt.want {
				t.Errorf("RenderOutline\n got = %q\nwant = %q", got, tt.want)
			}
			if !reflect.DeepEqual(ids, tt.wantIDs) {
				t.Errorf("RenderOutline ids = %v, want %v", ids, tt.wantIDs)
			}
		})
	}
}
