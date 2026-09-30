package cleanup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yasyf/daemonkit/durable"
)

// Journal is the durable store of job records: one private folder per job,
// one fsynced record in each. One serving daemon is its only writer.
type Journal struct {
	layout Layout
}

// Damaged is a job folder whose record could not be read. The journal reports
// it and touches nothing under it.
type Damaged struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// Switch is the queue-wide setting an operator controls.
type Switch struct {
	// Paused stops physical deletion of every job.
	Paused bool `json:"paused"`
}

// Validate accepts every Switch; the type has no invalid value.
func (Switch) Validate() error { return nil }

// OpenJournal readies layout's directories and returns its journal.
func OpenJournal(layout Layout) (*Journal, error) {
	if err := layout.Ensure(); err != nil {
		return nil, err
	}
	return &Journal{layout: layout}, nil
}

// ViewJournal returns layout's journal for a caller that only reads: it
// creates no directory and changes no mode, so the state tree may not exist.
func ViewJournal(layout Layout) (*Journal, error) {
	if err := layout.checkRoot(); err != nil {
		return nil, err
	}
	return &Journal{layout: layout}, nil
}

// Layout is the state tree the journal writes under.
func (j *Journal) Layout() Layout { return j.layout }

// Create makes job's private folder and publishes its first record.
func (j *Journal) Create(job Job) error {
	if err := j.check(job); err != nil {
		return err
	}
	dir := j.layout.JobDir(job.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("cleanup: create job folder: %w", err)
	}
	if err := durable.SyncDir(filepath.Dir(dir)); err != nil {
		return err
	}
	return j.write(job)
}

// Save republishes job's record atomically.
func (j *Journal) Save(job Job) error {
	if err := j.check(job); err != nil {
		return err
	}
	return j.write(job)
}

// Discard removes the record and folder of a job that holds nothing else: one
// refused before it mutated anything, or one whose payload is gone. A folder
// still holding a tree is an error and stays.
func (j *Journal) Discard(id string) error {
	if !jobIDPattern.MatchString(id) {
		return fmt.Errorf("cleanup: %q is not a job id", id)
	}
	dir := j.layout.JobDir(id)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cleanup: read job folder: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != recordName && !strings.HasPrefix(name, "."+recordName+".") {
			return fmt.Errorf("cleanup: job folder %s still holds %s", dir, name)
		}
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("cleanup: discard job record: %w", err)
		}
	}
	if err := durable.Remove(dir); err != nil {
		return fmt.Errorf("cleanup: discard job folder: %w", err)
	}
	return nil
}

// Load reads every job record in queue order. A folder whose record is torn,
// invalid, or names paths outside the folder it sits in comes back as Damaged
// rather than as a job. A folder with no record at all is one Create was
// interrupted in before anything was mutated, and is discarded when it holds
// nothing else.
func (j *Journal) Load() ([]Job, []Damaged, error) {
	entries, err := os.ReadDir(j.layout.JobsDir())
	if err != nil {
		return nil, nil, fmt.Errorf("cleanup: read jobs: %w", err)
	}
	var (
		jobs    []Job
		damaged []Damaged
	)
	for _, entry := range entries {
		id := entry.Name()
		job, err := durable.ReadFile[Job](j.layout.RecordPath(id))
		if errors.Is(err, fs.ErrNotExist) {
			if err := j.Discard(id); err != nil {
				damaged = append(damaged, Damaged{ID: id, Error: err.Error()})
			}
			continue
		}
		if err == nil {
			err = j.check(job)
		}
		if err == nil && job.ID != id {
			err = fmt.Errorf("record names job %s", job.ID)
		}
		if err != nil {
			damaged = append(damaged, Damaged{ID: id, Error: err.Error()})
			continue
		}
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].Seq < jobs[b].Seq })
	return jobs, damaged, nil
}

// LoadSwitch reads the queue-wide switch, unset when it was never written.
func (j *Journal) LoadSwitch() (Switch, error) {
	sw, err := durable.ReadFile[Switch](j.layout.SwitchPath())
	if errors.Is(err, fs.ErrNotExist) {
		return Switch{}, nil
	}
	return sw, err
}

// SaveSwitch publishes the queue-wide switch.
func (j *Journal) SaveSwitch(sw Switch) error {
	data, err := durable.Marshal(sw)
	if err != nil {
		return err
	}
	return durable.WriteFile(j.layout.SwitchPath(), data, 0o600)
}

func (j *Journal) check(job Job) error {
	if err := job.Validate(); err != nil {
		return fmt.Errorf("cleanup: job record: %w", err)
	}
	if job.Registered != j.layout.Registered(job.ID) || job.Payload != j.layout.Payload(job.ID) {
		return fmt.Errorf("cleanup: job %s names paths outside its folder under %s", job.ID, j.layout.JobsDir())
	}
	return nil
}

func (j *Journal) write(job Job) error {
	data, err := durable.Marshal(job)
	if err != nil {
		return err
	}
	return durable.WriteFile(j.layout.RecordPath(job.ID), data, 0o600)
}
