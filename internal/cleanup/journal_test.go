package cleanup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/daemonkit/durable"
)

func openJournal(t *testing.T) (*Journal, Layout) {
	t.Helper()
	l := Layout{Root: filepath.Join(t.TempDir(), "state")}
	j, err := OpenJournal(l)
	if err != nil {
		t.Fatalf("OpenJournal() = %v", err)
	}
	if j.Layout() != l {
		t.Fatalf("Layout() = %+v, want %+v", j.Layout(), l)
	}
	return j, l
}

func writeRecord(t *testing.T, l Layout, id string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(l.JobDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.RecordPath(id), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func marshalJob(t *testing.T, job Job) []byte {
	t.Helper()
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Lstat(%s) = %v, want not exist", path, err)
	}
}

func TestOpenJournalRejectsRoot(t *testing.T) {
	_, err := OpenJournal(Layout{Root: "state"})
	if want := `cleanup: layout root "state" is not a clean absolute path`; err == nil || err.Error() != want {
		t.Fatalf("OpenJournal() = %v, want %q", err, want)
	}
}

func TestViewJournalRejectsRoot(t *testing.T) {
	_, err := ViewJournal(Layout{Root: "state"})
	if want := `cleanup: layout root "state" is not a clean absolute path`; err == nil || err.Error() != want {
		t.Fatalf("ViewJournal() = %v, want %q", err, want)
	}
	assertAbsent(t, "state")
}

func TestViewJournalLeavesTheStateTreeAlone(t *testing.T) {
	tests := []struct {
		name string
		mode fs.FileMode
	}{
		{"absent root", 0},
		{"root open to its group", 0o750},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := Layout{Root: filepath.Join(t.TempDir(), "state")}
			if tt.mode != 0 {
				if err := os.Mkdir(l.Root, tt.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(l.Root, tt.mode); err != nil {
					t.Fatal(err)
				}
			}

			j, err := ViewJournal(l)
			if err != nil || j.Layout() != l {
				t.Fatalf("ViewJournal() = %+v, %v; want a journal over %+v", j, err, l)
			}

			if tt.mode == 0 {
				assertAbsent(t, l.Root)
				return
			}
			info, err := os.Lstat(l.Root)
			if err != nil || info.Mode() != fs.ModeDir|tt.mode {
				t.Errorf("root after ViewJournal = %v, %v; want %v", info, err, fs.ModeDir|tt.mode)
			}
			entries, err := os.ReadDir(l.Root)
			if err != nil || len(entries) != 0 {
				t.Errorf("root after ViewJournal holds %v, %v; want nothing", entries, err)
			}
		})
	}
}

func TestJournalRoundTripInQueueOrder(t *testing.T) {
	j, l := openJournal(t)
	first := validJob(l, "0000000000000003-cccccc", 1)
	first.Deferred, first.Phase, first.Owner = true, PhaseQueued, "stack"
	first.Head, first.RecoveryRef, first.Branch = "", "", ""
	second := validJob(l, "0000000000000001-aaaaaa", 2)
	second.Block(testCreated.Add(time.Minute), "activity", "vim holds the tree")
	third := validJob(l, "0000000000000002-bbbbbb", 3)
	third.Phase, third.Removed = PhaseDeleting, 17

	for _, job := range []Job{third, first, second} {
		if err := j.Create(job); err != nil {
			t.Fatalf("Create(%s) = %v", job.ID, err)
		}
		assertPrivateDir(t, l.JobDir(job.ID))
		info, err := os.Lstat(l.RecordPath(job.ID))
		if err != nil || info.Mode() != 0o600 {
			t.Fatalf("record %s = %v, %v; want -rw-------", job.ID, info, err)
		}
	}
	jobs, damaged, err := j.Load()
	if err != nil || len(damaged) != 0 {
		t.Fatalf("Load() = %v, %v", damaged, err)
	}
	if want := []Job{first, second, third}; !reflect.DeepEqual(jobs, want) {
		t.Fatalf("Load() jobs = %+v, want %+v", jobs, want)
	}

	third.Phase, third.Removed, third.Updated = PhaseDone, 90, testCreated.Add(time.Hour)
	if err := j.Save(third); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	jobs, damaged, err = j.Load()
	if err != nil || len(damaged) != 0 {
		t.Fatalf("Load() after Save = %v, %v", damaged, err)
	}
	if want := []Job{first, second, third}; !reflect.DeepEqual(jobs, want) {
		t.Fatalf("Load() after Save jobs = %+v, want %+v", jobs, want)
	}
	entries, err := os.ReadDir(l.JobDir(third.ID))
	if err != nil || len(entries) != 1 || entries[0].Name() != "job.json" {
		t.Errorf("job folder after Save = %v, %v; want only job.json", entries, err)
	}
}

func TestJournalRoundTripAdopted(t *testing.T) {
	j, l := openJournal(t)
	linked := validJob(l, "0000000000000001-aaaaaa", 1)
	adopted := adoptedJob(l, "0000000000000002-bbbbbb", 2)
	for _, job := range []Job{adopted, linked} {
		if err := j.Create(job); err != nil {
			t.Fatalf("Create(%s) = %v", job.ID, err)
		}
	}
	data, err := os.ReadFile(l.RecordPath(adopted.ID))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"adopted":      "true",
		"source":       `"` + testSource + `"`,
		"recovery_ref": `"` + testLegacyRef + `"`,
		"owner":        `"legacy quarantine import"`,
	} {
		if got := string(fields[key]); got != want {
			t.Errorf("record %s = %s, want %s", key, got, want)
		}
	}
	for _, key := range []string{"admin_dir", "deferred", "branch"} {
		if raw, ok := fields[key]; ok {
			t.Errorf("record carries %s = %s, want it omitted", key, raw)
		}
	}

	jobs, damaged, err := j.Load()
	if err != nil || len(damaged) != 0 {
		t.Fatalf("Load() = %v, %v", damaged, err)
	}
	if want := []Job{linked, adopted}; !reflect.DeepEqual(jobs, want) {
		t.Fatalf("Load() jobs = %+v, want %+v", jobs, want)
	}

	adopted.Phase, adopted.Updated = PhaseUnregistered, testCreated.Add(time.Minute)
	if err := j.Save(adopted); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	moved := adopted
	moved.Phase = PhaseMoved
	if err := j.Save(moved); err == nil || err.Error() != `cleanup: job record: phase "moved" belongs to a registered worktree, not an adopted tree` {
		t.Fatalf("Save() at moved = %v, want the adopted phase refusal", err)
	}
	jobs, damaged, err = j.Load()
	if err != nil || len(damaged) != 0 {
		t.Fatalf("Load() after Save = %v, %v", damaged, err)
	}
	if want := []Job{linked, adopted}; !reflect.DeepEqual(jobs, want) {
		t.Fatalf("Load() after Save jobs = %+v, want %+v", jobs, want)
	}
}

func TestJournalLoadDamagedAdopted(t *testing.T) {
	j, l := openJournal(t)
	withAdmin := adoptedJob(l, "0000000000000001-aaaaaa", 1)
	withAdmin.AdminDir, withAdmin.Admin = "/repo/.git/worktrees/feature", FileID{Dev: 1, Ino: 2}
	rebound := adoptedJob(l, "0000000000000002-bbbbbb", 2)
	rebound.Source = "/home/u/.cache/codex-worktree-trash-20260928/ffffffffffffffffffff-feature"
	unadopted := adoptedJob(l, "0000000000000003-cccccc", 3)
	unadopted.Adopted = false
	records := map[string]struct {
		job  Job
		want string
	}{
		withAdmin.ID: {withAdmin, "validate: an adopted job carries admin or link state it never had"},
		rebound.ID:   {rebound, `validate: source "` + rebound.Source + `" is not the tree ` + testLegacyRef + ` pins`},
		unadopted.ID: {unadopted, `validate: source "` + testSource + `" set on a job that adopted nothing`},
	}
	for id, r := range records {
		writeRecord(t, l, id, marshalJob(t, r.job))
	}

	jobs, damaged, err := j.Load()
	if err != nil || len(jobs) != 0 {
		t.Fatalf("Load() = %+v, %v; want no jobs", jobs, err)
	}
	if len(damaged) != len(records) {
		t.Fatalf("Load() damaged = %+v, want %d entries", damaged, len(records))
	}
	for _, d := range damaged {
		if r, ok := records[d.ID]; !ok || !strings.Contains(d.Error, r.want) {
			t.Errorf("damaged %s = %q, want error containing %q", d.ID, d.Error, r.want)
		}
	}
}

func TestJournalCreateRefuses(t *testing.T) {
	j, l := openJournal(t)
	invalid := validJob(l, testID, 0)
	elsewhere := validJob(Layout{Root: "/elsewhere"}, testID, 1)
	tests := []struct {
		name string
		job  Job
		want string
	}{
		{"invalid record", invalid, "cleanup: job record: seq is zero"},
		{"paths outside its folder", elsewhere, "cleanup: job " + testID + " names paths outside its folder under " + l.JobsDir()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := j.Create(tt.job)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("Create() = %v, want %q", err, tt.want)
			}
			assertAbsent(t, l.JobDir(testID))
		})
	}
}

func TestJournalCreateExisting(t *testing.T) {
	j, l := openJournal(t)
	job := validJob(l, testID, 1)
	if err := j.Create(job); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	again := job
	again.Seq = 2
	if err := j.Create(again); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second Create() = %v, want fs.ErrExist", err)
	}
	jobs, _, err := j.Load()
	if err != nil || !reflect.DeepEqual(jobs, []Job{job}) {
		t.Errorf("Load() = %+v, %v; want the first record", jobs, err)
	}
}

func TestJournalSaveRefusesInvalid(t *testing.T) {
	j, l := openJournal(t)
	job := validJob(l, testID, 1)
	if err := j.Create(job); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	before, err := os.ReadFile(l.RecordPath(testID))
	if err != nil {
		t.Fatal(err)
	}
	bad := job
	bad.Payload = l.Registered(testID)
	if err := j.Save(bad); err == nil || !strings.Contains(err.Error(), "cleanup: job record: payload") {
		t.Fatalf("Save() = %v, want a job record error", err)
	}
	after, err := os.ReadFile(l.RecordPath(testID))
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("record after refused Save = %q, %v; want %q", after, err, before)
	}
}

func TestJournalLoadDamaged(t *testing.T) {
	j, l := openJournal(t)
	good := validJob(l, "0000000000000009-999999", 1)
	if err := j.Create(good); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	valid := marshalJob(t, validJob(l, "0000000000000001-aaaaaa", 1))
	unseq := validJob(l, "0000000000000005-eeeeee", 1)
	unseq.Seq = 0
	elsewhere := validJob(Layout{Root: "/elsewhere"}, "0000000000000006-ffffff", 1)
	elsewhereData, err := durable.Marshal(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	impostorData, err := durable.Marshal(validJob(l, "0000000000000008-888888", 1))
	if err != nil {
		t.Fatal(err)
	}
	records := []struct {
		id   string
		data []byte
		want string
	}{
		{"0000000000000001-aaaaaa", append([]byte(`{"extra":1,`), valid[1:]...), `json: unknown field "extra"`},
		{"0000000000000002-bbbbbb", append(append([]byte(nil), valid...), "\n{}\n"...), "decode: trailing JSON"},
		{"0000000000000003-cccccc", append([]byte(`{"schema":1,`), valid[1:]...), `decode: duplicate object key "schema"`},
		{"0000000000000004-dddddd", valid[:len(valid)/2], "decode: unexpected EOF"},
		{"0000000000000005-eeeeee", marshalJob(t, unseq), "validate: seq is zero"},
		{"0000000000000006-ffffff", elsewhereData, "cleanup: job 0000000000000006-ffffff names paths outside its folder under " + l.JobsDir()},
		{"0000000000000007-777777", impostorData, "record names job 0000000000000008-888888"},
	}
	for _, r := range records {
		writeRecord(t, l, r.id, r.data)
	}
	if err := os.MkdirAll(l.Payload("000000000000000a-aaaaaa"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(l.JobsDir(), "junk"), 0o700); err != nil {
		t.Fatal(err)
	}

	jobs, damaged, err := j.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if !reflect.DeepEqual(jobs, []Job{good}) {
		t.Errorf("Load() jobs = %+v, want only %s", jobs, good.ID)
	}
	want := map[string]string{
		"000000000000000a-aaaaaa": "cleanup: job folder " + l.JobDir("000000000000000a-aaaaaa") + " still holds payload",
		"junk":                    `cleanup: "junk" is not a job id`,
	}
	for _, r := range records {
		want[r.id] = r.want
	}
	if len(damaged) != len(want) {
		t.Fatalf("Load() damaged = %+v, want %d entries", damaged, len(want))
	}
	for _, d := range damaged {
		if w, ok := want[d.ID]; !ok || !strings.Contains(d.Error, w) {
			t.Errorf("damaged %s = %q, want error containing %q", d.ID, d.Error, w)
		}
	}
	for _, r := range records {
		data, err := os.ReadFile(l.RecordPath(r.id))
		if err != nil || !bytes.Equal(data, r.data) {
			t.Errorf("record %s after Load = %q, %v; want untouched", r.id, data, err)
		}
	}
	for _, dir := range []string{l.Payload("000000000000000a-aaaaaa"), filepath.Join(l.JobsDir(), "junk")} {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			t.Errorf("%s after Load = %v, %v; want untouched directory", dir, info, err)
		}
	}
}

func TestJournalLoadDiscardsInterruptedCreate(t *testing.T) {
	j, l := openJournal(t)
	empty := "0000000000000001-aaaaaa"
	stump := "0000000000000002-bbbbbb"
	if err := os.Mkdir(l.JobDir(empty), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRecord(t, l, stump, nil)
	if err := os.Rename(l.RecordPath(stump), filepath.Join(l.JobDir(stump), ".job.json.123456")); err != nil {
		t.Fatal(err)
	}

	jobs, damaged, err := j.Load()
	if err != nil || len(jobs) != 0 || len(damaged) != 0 {
		t.Fatalf("Load() = %+v, %+v, %v; want nothing", jobs, damaged, err)
	}
	assertAbsent(t, l.JobDir(empty))
	assertAbsent(t, l.JobDir(stump))
}

func TestJournalDiscard(t *testing.T) {
	j, l := openJournal(t)
	job := validJob(l, testID, 1)
	if err := j.Create(job); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if err := os.WriteFile(filepath.Join(l.JobDir(testID), ".job.json.654321"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := j.Discard(testID); err != nil {
		t.Fatalf("Discard() = %v", err)
	}
	assertAbsent(t, l.JobDir(testID))
	assertPrivateDir(t, l.JobsDir())
}

func TestJournalDiscardRefusesPayload(t *testing.T) {
	j, l := openJournal(t)
	job := validJob(l, testID, 1)
	if err := j.Create(job); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if err := os.Mkdir(l.Payload(testID), 0o700); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(l.Payload(testID), "kept.txt")
	if err := os.WriteFile(kept, []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := j.Discard(testID)
	if want := "cleanup: job folder " + l.JobDir(testID) + " still holds payload"; err == nil || err.Error() != want {
		t.Fatalf("Discard() = %v, want %q", err, want)
	}
	if data, err := os.ReadFile(kept); err != nil || string(data) != "work" {
		t.Errorf("payload file after Discard = %q, %v; want \"work\"", data, err)
	}
	jobs, damaged, err := j.Load()
	if err != nil || len(damaged) != 0 || !reflect.DeepEqual(jobs, []Job{job}) {
		t.Errorf("Load() after refused Discard = %+v, %+v, %v; want the record intact", jobs, damaged, err)
	}
}

func TestJournalDiscardAbsent(t *testing.T) {
	j, l := openJournal(t)
	if err := j.Discard(testID); err != nil {
		t.Fatalf("Discard() of an absent job = %v, want nil", err)
	}
	assertPrivateDir(t, l.JobsDir())
}

func TestJournalDiscardRejectsID(t *testing.T) {
	j, l := openJournal(t)
	if err := os.Mkdir(filepath.Join(l.Root, "victim"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := j.Discard("../victim")
	if want := `cleanup: "../victim" is not a job id`; err == nil || err.Error() != want {
		t.Fatalf("Discard() = %v, want %q", err, want)
	}
	assertPrivateDir(t, filepath.Join(l.Root, "victim"))
}

func TestJournalSwitch(t *testing.T) {
	j, l := openJournal(t)
	sw, err := j.LoadSwitch()
	if err != nil || sw != (Switch{}) {
		t.Fatalf("LoadSwitch() unset = %+v, %v; want zero, nil", sw, err)
	}
	assertAbsent(t, l.SwitchPath())
	for _, want := range []Switch{{Paused: true}, {Paused: false}} {
		if err := j.SaveSwitch(want); err != nil {
			t.Fatalf("SaveSwitch(%+v) = %v", want, err)
		}
		got, err := j.LoadSwitch()
		if err != nil || got != want {
			t.Fatalf("LoadSwitch() = %+v, %v; want %+v", got, err, want)
		}
	}
	info, err := os.Lstat(l.SwitchPath())
	if err != nil || info.Mode() != 0o600 {
		t.Errorf("switch file = %v, %v; want -rw-------", info, err)
	}
}

func TestJournalSwitchStrict(t *testing.T) {
	j, l := openJournal(t)
	if err := os.WriteFile(l.SwitchPath(), []byte(`{"paused":true,"extra":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.LoadSwitch(); err == nil || !strings.Contains(err.Error(), `json: unknown field "extra"`) {
		t.Fatalf("LoadSwitch() = %v, want unknown field error", err)
	}
}
