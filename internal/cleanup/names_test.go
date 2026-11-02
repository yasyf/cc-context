package cleanup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNamesNeverForgetAPathOrItsLaneName(t *testing.T) {
	first := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	cut := Use{Path: "/Users/u/.claude/worktrees/cc-context/engine", By: "worktree add", At: first}
	tests := []struct {
		name string
		path string
		want Use
		used bool
	}{
		{"the same path", cut.Path, cut, true},
		{"the same lane under another worktree root", "/Users/u/repos/claude/worktrees/cc-context/engine", cut, true},
		{"the same lane name in another repository", "/Users/u/repos/claude/worktrees/monorepo/engine", Use{}, false},
		{"another lane in the same repository", "/Users/u/repos/claude/worktrees/cc-context/engine-2", Use{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			names := Layout{Root: t.TempDir()}.Names()
			if err := names.Record(cut.Path, cut.By, cut.At); err != nil {
				t.Fatalf("Record() = %v", err)
			}
			if err := names.Record(cut.Path, "cleanup job 1", first.Add(time.Hour)); err != nil {
				t.Fatalf("second Record() = %v", err)
			}
			got, used, err := names.Used(tt.path)
			if err != nil || used != tt.used || got != tt.want {
				t.Errorf("Used(%s) = %+v, %t, %v; want %+v, %t", tt.path, got, used, err, tt.want, tt.used)
			}
		})
	}
}

func TestNamesBeforeAnyRecordUseNothing(t *testing.T) {
	names := Layout{Root: filepath.Join(t.TempDir(), "absent")}.Names()
	if got, used, err := names.Used("/Users/u/.claude/worktrees/cc-context/engine"); err != nil || used {
		t.Errorf("Used() on an absent index = %+v, %t, %v; want unused", got, used, err)
	}
}

func TestNamesLeaveNoStagedFileBehind(t *testing.T) {
	layout := Layout{Root: t.TempDir()}
	if err := layout.Names().Record("/Users/u/.claude/worktrees/cc-context/engine", "stack new", time.Now()); err != nil {
		t.Fatalf("Record() = %v", err)
	}
	shards, err := os.ReadDir(filepath.Join(layout.Root, "names"))
	if err != nil {
		t.Fatal(err)
	}
	entries := 0
	for _, shard := range shards {
		files, err := os.ReadDir(filepath.Join(layout.Root, "names", shard.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if filepath.Ext(f.Name()) == ".tmp" {
				t.Errorf("staged file %s left behind", f.Name())
			}
			entries++
		}
	}
	if entries != 2 {
		t.Errorf("index holds %d entries, want 2: the path and its lane name", entries)
	}
}

func TestJournalCreateRecordsTheWorktreePath(t *testing.T) {
	j, layout := openJournal(t)
	job := validJob(layout, testID, 1)
	if err := j.Create(job); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	got, used, err := layout.Names().Used(job.Original)
	if err != nil || !used || got != (Use{Path: job.Original, By: "cleanup job " + job.ID, At: job.Created}) {
		t.Errorf("Used(%s) = %+v, %t, %v; want the job's use", job.Original, got, used, err)
	}
}
