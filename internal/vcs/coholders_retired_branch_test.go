package vcs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

func TestCoHoldersRetiredBranchChangeDuringCensus(t *testing.T) {
	fixture, _ := newRetiredCensusFixture(t, 1)
	head := filepath.Join(fixture.Checkout.GitDir, "HEAD")
	replacement := head + ".next"
	mustWriteFile(t, replacement, "ref: refs/heads/"+fixture.Branch+"\n")
	if err := os.Remove(head); err != nil {
		t.Fatal(err)
	}
	if _, err := render.RunCLI(t.Context(), render.Ambient, "mkfifo", []string{head}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	written := make(chan error, 1)
	go func() {
		writer, err := os.OpenFile(head, os.O_WRONLY, 0)
		if err != nil {
			written <- err
			return
		}
		_, writeErr := writer.WriteString("ref: refs/heads/previous-branch\n")
		renameErr := os.Rename(replacement, head)
		closeErr := writer.Close()
		written <- errors.Join(writeErr, renameErr, closeErr)
	}()
	branch, holders, err := CoHolders(t.Context(), fixture.Checkout)
	if writeErr := <-written; writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil || branch != fixture.Branch || !reflect.DeepEqual(holders, fixture.Holders) {
		t.Fatalf("CoHolders after current HEAD changed = %q, %q, %v; want %q, %q, nil", branch, holders, err, fixture.Branch, fixture.Holders)
	}
}

func TestRetiredHoldingsRetainsUnrelatedBranches(t *testing.T) {
	fixture, _ := newRetiredCensusFixture(t, 2)
	t.Setenv("PATH", t.TempDir())
	want := []Holding{{Path: fixture.Holders[0], Branch: fixture.Branch}, {Path: fixture.Holders[1], Branch: fixture.Branch}}
	for index, root := range fixture.Unrelated {
		want = append(want, Holding{Path: root, Branch: fmt.Sprintf("unrelated-%04d", index)})
	}
	holdings, err := retiredHoldings(fixture.Checkout.CommonDir)
	if err != nil || !reflect.DeepEqual(holdings, want) {
		t.Fatalf("full retiredHoldings = %+v, %v; want %+v, nil", holdings, err, want)
	}
}
