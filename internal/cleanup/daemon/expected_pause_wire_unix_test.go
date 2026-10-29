//go:build !windows

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanup/relocate"
)

func serveExpectedGitFixture(t *testing.T, f *gitFixture, sw cleanup.Switch) *Client {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "ccx-expected-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove daemon fixture: %v", err)
		}
	})
	f.journal, err = cleanup.OpenJournal(cleanup.Layout{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.journal.SaveSwitch(sw); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := listen(ctx, f.engine(), f.journal.Layout())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := f.await(served, "Serve()"); err != nil {
			t.Errorf("Serve() = %v", err)
		}
	})
	socket, err := f.journal.Layout().Socket()
	if err != nil {
		t.Fatal(err)
	}
	return Dial(socket)
}

func TestPausedDeferPreservesExpectedRegistrationOverTheWire(t *testing.T) {
	f := newGitFixture(t)
	request := f.deferral()
	kept := filepath.Join(filepath.Dir(f.worktree), "original")
	f.run(f.repo, "worktree", "move", f.worktree, kept)
	f.run(f.repo, "worktree", "add", "-q", "--detach", f.worktree)
	marker := filepath.Join(f.worktree, "replacement.txt")
	f.write(marker, "replacement\n")
	replacement, err := relocate.Observe(f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	listing := f.listing()
	client := serveExpectedGitFixture(t, f, cleanup.Switch{Paused: true})

	receipt, err := client.Defer(context.Background(), request)
	var refused *cleanup.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "replaced" || receipt != (cleanup.Receipt{}) {
		t.Fatalf("Defer() = %+v, %v; want replaced refusal without a receipt", receipt, err)
	}
	jobs, damaged, err := f.journal.Load()
	if err != nil || len(damaged) != 0 || len(jobs) != 0 {
		t.Errorf("Load() = %+v, damaged %v, %v; want no job", jobs, damaged, err)
	}
	if got, err := relocate.Observe(f.worktree); err != nil || got != replacement {
		t.Errorf("replacement registration = %+v, %v; want %+v", got, err, replacement)
	}
	if got, err := relocate.Observe(kept); err != nil || got != request.Expected {
		t.Errorf("original registration = %+v, %v; want %+v", got, err, request.Expected)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "replacement\n" {
		t.Errorf("replacement contents = %q, %v", got, err)
	}
	if got := f.listing(); got != listing {
		t.Errorf("registrations = %q, want %q", got, listing)
	}
	if got := f.gitCalls(); got != "" {
		t.Errorf("daemon git calls = %q, want none", got)
	}
}
