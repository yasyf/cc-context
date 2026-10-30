package vcs

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestHoldingCensusPreservesExternalCheckoutResults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		head    string
		invalid bool
	}{
		{"symbolic", "ref: refs/heads/external\n", false},
		{"detached", strings.Repeat("a", 40) + "\n", false},
		{"invalid", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := vcstest.Repo(t, vcstest.Trunk("trunk"))
			root, admin := filepath.Join(t.TempDir(), "checkout"), filepath.Join(t.TempDir(), "admin")
			mustMkdir(t, root)
			mustMkdir(t, admin)
			mustWriteFile(t, filepath.Join(root, ".git"), gitdirPrefix+admin+"\n")
			mustWriteFile(t, filepath.Join(admin, "commondir"), filepath.Join(f.Dir, ".git")+"\n")
			mustWriteFile(t, filepath.Join(admin, "gitdir"), filepath.Join(root, ".git")+"\n")
			if tc.invalid {
				mustMkdir(t, filepath.Join(admin, "HEAD"))
			} else {
				mustWriteFile(t, filepath.Join(admin, "HEAD"), tc.head)
			}
			checkout, err := ResolveCheckout(root)
			if err != nil {
				t.Fatal(err)
			}
			census, err := CaptureHoldings(f.Context(), checkout)
			if err != nil {
				t.Fatal(err)
			}
			want, wantErr := BranchHolders(f.Context(), checkout)
			if (wantErr != nil) != tc.invalid {
				t.Fatalf("standalone error = %v, want invalid %v", wantErr, tc.invalid)
			}
			got, gotErr := census.BranchHolders(f.Context(), checkout)
			if wantErr != nil {
				if gotErr == nil || gotErr.Error() != wantErr.Error() {
					t.Fatalf("census error = %v, want %v", gotErr, wantErr)
				}
			} else if gotErr != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("census holders = %+v, %v; want %+v", got, gotErr, want)
			}
			main, err := ResolveCheckout(f.Dir)
			if err != nil {
				t.Fatal(err)
			}
			want, err = BranchHolders(f.Context(), main)
			if err != nil {
				t.Fatal(err)
			}
			got, err = census.BranchHolders(f.Context(), main)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("shared holders changed = %+v, %v; want %+v", got, err, want)
			}
		})
	}
}
