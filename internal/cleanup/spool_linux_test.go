package cleanup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestJobsForPicksTheSpoolOfAVolumeThisUserCanKeepOne(t *testing.T) {
	l := Layout{Root: filepath.Join(t.TempDir(), "state")}
	shm, err := nearestDev("/dev/shm")
	if err != nil {
		t.Fatal(err)
	}
	proc, err := nearestDev("/proc/self")
	if err != nil {
		t.Fatal(err)
	}
	home, err := nearestDev(l.JobsDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		tree string
		dev  uint64
		want string
	}{
		{"/dev/shm/tree", shm, Spool("/dev/shm")},
		{"/proc/self/tree", proc, l.JobsDir()},
		{filepath.Join(l.Root, "tree"), home, l.JobsDir()},
	} {
		if got := l.JobsFor(c.tree, c.dev); got != c.want {
			t.Errorf("JobsFor(%s) = %s, want %s", c.tree, got, c.want)
		}
	}
}

func TestJournalKeepsASpooledJobAreaOnItsTreeVolume(t *testing.T) {
	l := Layout{Root: filepath.Join(t.TempDir(), "state")}
	j, err := OpenJournal(l)
	if err != nil {
		t.Fatal(err)
	}
	id := MintID(time.Now())
	job := validJob(l, id, 1)
	job.Registered, job.Payload = Area(Spool("/dev/shm"), id)
	area := filepath.Dir(job.Payload)
	t.Cleanup(func() { _ = os.RemoveAll(area) })

	if err := j.Create(job); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := private(area); err != nil {
		t.Errorf("job area: %v", err)
	}
	jobs, damaged, err := j.Load()
	if err != nil || len(damaged) != 0 || len(jobs) != 1 || !reflect.DeepEqual(jobs[0], job) {
		t.Fatalf("Load() = %+v, damaged %v, error %v", jobs, damaged, err)
	}

	foreign := job
	foreign.Registered, foreign.Payload = Area("/dev/shm/elsewhere", id)
	if err := j.Save(foreign); err == nil {
		t.Error("Save() accepted a job area outside the spool")
	}

	left := filepath.Join(area, "left")
	if err := os.WriteFile(left, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := j.Discard(job); err == nil {
		t.Error("Discard() removed a job whose area still holds a file")
	}
	if err := os.Remove(left); err != nil {
		t.Fatal(err)
	}
	if err := j.Discard(job); err != nil {
		t.Fatalf("Discard() error = %v", err)
	}
	for _, gone := range []string{area, l.JobDir(id)} {
		if _, err := os.Lstat(gone); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s survived Discard(): %v", gone, err)
		}
	}
}
