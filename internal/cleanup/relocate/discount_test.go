package relocate

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
)

var retiring = []cleanup.ProcessID{{PID: 501, Start: 1790000000}, {PID: 733, Start: 1790000450}}

func (f *fixture) watched() {
	f.watchers.retiring = func(context.Context, string) ([]cleanup.ProcessID, error) { return retiring, nil }
}

func (f *fixture) consulted(guarded []string, discounts [][]cleanup.ProcessID, named []string) {
	f.t.Helper()
	if !reflect.DeepEqual(f.guarded, guarded) {
		f.t.Errorf("guard consulted for %v, want %v", f.guarded, guarded)
	}
	if !reflect.DeepEqual(f.discounts, discounts) {
		f.t.Errorf("guard calls carried the discounts %v, want %v", f.discounts, discounts)
	}
	if !reflect.DeepEqual(f.watchers.named, named) {
		f.t.Errorf("watchers named for %v, want %v", f.watchers.named, named)
	}
}

func (f *fixture) retiredUnder(released [][]cleanup.ProcessID) {
	f.t.Helper()
	if !reflect.DeepEqual(f.watchers.released, released) {
		f.t.Errorf("retirements carried the discounts %v, want %v", f.watchers.released, released)
	}
}

func TestGuardDiscountsOnlyWatchersNotYetRetired(t *testing.T) {
	t.Run("a preview", func(t *testing.T) {
		f := newFixture(t)
		f.watched()

		if _, err := f.relocator.Preview(context.Background(), f.request(false)); err != nil {
			t.Fatalf("Preview() error = %v", err)
		}

		f.consulted([]string{f.worktree}, [][]cleanup.ProcessID{retiring}, []string{f.worktree})
	})

	t.Run("an immediate removal", func(t *testing.T) {
		f := newFixture(t)
		f.watched()
		job := f.accept()
		f.advance(&job)
		f.finished(&job)

		f.admit(&job)

		f.consulted(
			[]string{f.worktree, f.worktree, job.Registered, job.Payload},
			[][]cleanup.ProcessID{retiring, nil, nil, nil},
			[]string{f.worktree, f.worktree},
		)
		f.retiredUnder([][]cleanup.ProcessID{retiring})
		if want := []string{f.worktree}; !reflect.DeepEqual(f.watchers.retired, want) {
			t.Errorf("watchers retired %v, want %v", f.watchers.retired, want)
		}
	})

	t.Run("a deferred removal", func(t *testing.T) {
		f := newFixture(t)
		f.watched()
		job := f.intend()
		f.holdAt(f.worktree)
		f.advance(&job)
		f.advance(&job)
		if job.Phase != cleanup.PhaseWaiting || len(f.watchers.retired) != 0 {
			t.Fatalf("job is at %s with %v retired, want waiting with nothing retired", job.Phase, f.watchers.retired)
		}
		f.release()
		f.advance(&job)
		f.finished(&job)

		f.admit(&job)

		f.consulted(
			[]string{f.worktree, f.worktree, f.worktree, f.worktree, f.worktree, job.Registered, job.Payload},
			[][]cleanup.ProcessID{retiring, retiring, retiring, retiring, nil, nil, nil},
			[]string{f.worktree, f.worktree, f.worktree, f.worktree, f.worktree},
		)
		f.retiredUnder([][]cleanup.ProcessID{retiring})
	})

	t.Run("a watcher that did not let go blocks the move", func(t *testing.T) {
		f := newFixture(t)
		f.watched()
		holder := &cleanup.ActiveError{
			Worktree: f.worktree,
			Holders:  []cleanup.Holder{{PID: 501, Name: "watchman", Evidence: cleanup.EvidenceFD, Path: f.worktree}},
		}
		f.guard = func(ctx context.Context, _ string) error {
			if len(cleanup.RetiringFrom(ctx)) == 0 {
				return holder
			}
			return nil
		}
		job := f.accept()

		f.advance(&job)

		f.blocked(&job, cleanup.PhasePrepared, "activity", holder.Error())
		if got := f.id(f.worktree); got != job.Tree {
			t.Errorf("worktree identity = %v, want the tree %v", got, job.Tree)
		}
		f.absent(job.Registered)
		f.consulted([]string{f.worktree, f.worktree}, [][]cleanup.ProcessID{retiring, nil}, []string{f.worktree, f.worktree})
		f.retiredUnder([][]cleanup.ProcessID{retiring})
	})

	t.Run("the watchers named at retirement are the ones discounted", func(t *testing.T) {
		f := newFixture(t)
		job := f.accept()
		later := []cleanup.ProcessID{{PID: 907, Start: 1790000900}}
		f.watchers.retiring = func(context.Context, string) ([]cleanup.ProcessID, error) { return later, nil }

		f.advance(&job)

		f.finished(&job)
		f.consulted(
			[]string{f.worktree, f.worktree, job.Registered},
			[][]cleanup.ProcessID{nil, nil, nil},
			[]string{f.worktree, f.worktree},
		)
		f.retiredUnder([][]cleanup.ProcessID{later})
	})

	t.Run("an adoption", func(t *testing.T) {
		q := newQuarantine(t)
		q.watched()
		job := q.adopt()
		q.advance(&job)
		q.finished(&job)

		q.admit(&job)

		q.consulted([]string{q.source, q.source, job.Payload}, [][]cleanup.ProcessID{nil, nil, nil}, nil)
		q.retiredUnder(nil)
		if len(q.watchers.retired) != 0 {
			t.Errorf("watchers retired %v, want none", q.watchers.retired)
		}
	})
}

func TestUnnamedWatchersAuthorizeNothing(t *testing.T) {
	unreadable := errors.New("watcher registry unreadable")
	unnamed := func(f *fixture) {
		f.watchers.retiring = func(context.Context, string) ([]cleanup.ProcessID, error) { return nil, unreadable }
	}

	t.Run("accept refuses", func(t *testing.T) {
		f := newFixture(t)
		unnamed(f)

		_, err := f.relocator.Accept(context.Background(), 1, f.request(false))

		var (
			refused *cleanup.RefusedError
			active  *cleanup.ActiveError
		)
		if !errors.Is(err, unreadable) || errors.As(err, &refused) || errors.As(err, &active) {
			t.Fatalf("Accept() error = %v, want a plain error wrapping the watchers'", err)
		}
		if want := "cleanup: could not verify that " + f.worktree + " is idle: name the watchers of " + f.worktree + ": watcher registry unreadable"; err.Error() != want {
			t.Errorf("Accept() error = %q, want %q", err, want)
		}
		f.unjournaled()
		f.consulted(nil, nil, []string{f.worktree})
	})

	t.Run("intend waits without a verdict", func(t *testing.T) {
		f := newFixture(t)
		unnamed(f)

		job := f.intend()

		want := []cleanup.JobError{{
			At:      f.clock,
			Phase:   cleanup.PhaseWaiting,
			Message: "could not verify that " + f.worktree + " is idle: name the watchers of " + f.worktree + ": watcher registry unreadable",
		}}
		if job.Phase != cleanup.PhaseWaiting || !reflect.DeepEqual(job.Errors, want) {
			t.Errorf("Intend() journaled %s with errors %+v, want waiting with %+v", job.Phase, job.Errors, want)
		}
		f.stored(&job)
		f.consulted(nil, nil, []string{f.worktree})
	})

	t.Run("a deferred recheck blocks", func(t *testing.T) {
		f := newFixture(t)
		job := f.intend()
		unnamed(f)

		f.advance(&job)

		f.blocked(&job, cleanup.PhaseQueued, "activity", "could not verify that "+f.worktree+" is idle: name the watchers of "+f.worktree+": watcher registry unreadable")
		if got := f.id(f.worktree); got != job.Tree {
			t.Errorf("worktree identity = %v, want the tree %v", got, job.Tree)
		}
		f.absent(job.Registered)
		if len(f.watchers.retired) != 0 {
			t.Errorf("watchers retired %v, want none", f.watchers.retired)
		}
		f.consulted([]string{f.worktree}, [][]cleanup.ProcessID{nil}, []string{f.worktree, f.worktree})
	})
}
