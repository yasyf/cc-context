package cli

import (
	"testing"
	"time"
)

// The activity comments below are Graphite's, copied out of the pull requests
// named in each constant — the queue publishes no schema for them, so the
// wording a parser keys on is only ever a real one.
const (
	mqActivityQueued = "### Merge activity\n\n" +
		"* **Aug 27, 10:49 AM UTC**: `yasyf` added this pull request to the [Graphite merge queue](https://app.graphite.com/merges?org=Forge-AI&repo=monorepo).\n" +
		"* **Aug 27, 10:50 AM UTC**: CI is running for this pull request on a draft pull request ([#16560](https://app.graphite.com/github/pr/Forge-AI/monorepo/16560)) due to your merge queue CI optimization settings.\n" +
		"* **Aug 27, 11:17 AM UTC**: CI is running for this pull request on a draft pull request ([#16565](https://app.graphite.com/github/pr/Forge-AI/monorepo/16565)) due to your merge queue CI optimization settings.\n"

	mqActivityRejected = "### Merge activity\n\n" +
		"* **Aug 27, 10:51 AM UTC**: `yasyf` added this pull request to the [Graphite merge queue](https://app.graphite.com/merges?org=Forge-AI&repo=monorepo).\n" +
		"* **Aug 27, 10:52 AM UTC**: CI is running for this pull request on a draft pull request ([#16562](https://app.graphite.com/github/pr/Forge-AI/monorepo/16562)) due to your merge queue CI optimization settings.\n" +
		"* **Aug 27, 11:16 AM UTC**: The [Graphite merge queue](https://app.graphite.com/merges?org=Forge-AI&repo=monorepo) removed this pull request due to **downstack failures on PR #[16372](https://app.graphite.com/github/pr/Forge-AI/monorepo/16372)**.\n"

	mqActivityPending = "### Merge activity\n\n" +
		"* **Aug 27, 11:17 AM UTC**: The merge label 'merge' was detected. This PR will be added to the [Graphite merge queue](https://app.graphite.com/merges?org=Forge-AI&repo=monorepo) once it meets the requirements.\n"
)

func mqAt(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return at
}

// TestMergeQueue holds the reconstruction against timelines GitHub actually
// served. The first case is the one this command exists for: Forge-AI/monorepo
// #16535 was labelled merge-fast at 09:05, force-pushed at 09:18, and landed
// the commit the label caught — 861f15e6 — leaving 8259d97b behind.
func TestMergeQueue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      func(t *testing.T) mqInput
		phase   mqPhase
		label   string
		draft   int
		held    string
		dropped []string
		nilOut  bool
	}{
		{
			name: "a push after the label is a commit the queue will not land",
			in: func(t *testing.T) mqInput {
				return mqInput{
					Head:   "8259d97b",
					Landed: true,
					Commits: []mqCommit{
						{OID: "8259d97b", Subject: "the nix canary reports", At: mqAt(t, "2026-08-27T09:18:07Z")},
					},
					Pushes: []mqForcePush{{At: mqAt(t, "2026-08-27T09:18:14Z"), Before: "861f15e6"}},
					Events: []mqLabelEvent{
						{At: mqAt(t, "2026-08-27T09:05:34Z"), Name: mqLabelFast, Actor: "yasyf", Added: true},
						{At: mqAt(t, "2026-08-27T09:26:22Z"), Name: mqLabelFast, Actor: graphiteQueueActor},
					},
				}
			},
			phase:   mqPhaseMerged,
			label:   mqLabelFast,
			held:    "861f15e6",
			dropped: []string{"8259d97b"},
		},
		{
			name: "a re-formed draft snapshots the branch again",
			in: func(t *testing.T) mqInput {
				return mqInput{
					Head: "b430aabb",
					Commits: []mqCommit{
						{OID: "b430aabb", Subject: "cover the run-filter math", At: mqAt(t, "2026-08-27T11:11:09Z")},
					},
					Pushes:   []mqForcePush{{At: mqAt(t, "2026-08-27T11:13:12Z"), Before: "0f177214"}},
					Labels:   []string{mqLabel},
					Activity: mqActivityQueued,
					Drafts: map[int]time.Time{
						16560: mqAt(t, "2026-08-27T10:50:00Z"),
						16565: mqAt(t, "2026-08-27T11:17:00Z"),
					},
				}
			},
			phase: mqPhaseQueued,
			label: mqLabel,
			draft: 16565,
			held:  "b430aabb",
		},
		{
			name: "a rejection is not a consumed label",
			in: func(t *testing.T) mqInput {
				return mqInput{
					Head:     "4f1a6643",
					Activity: mqActivityRejected,
					Events: []mqLabelEvent{
						{At: mqAt(t, "2026-08-27T10:58:00Z"), Name: mqLabel, Actor: "yasyf", Added: true},
						{At: mqAt(t, "2026-08-27T11:17:19Z"), Name: mqLabel, Actor: "yasyf"},
					},
				}
			},
			phase: mqPhaseRejected,
		},
		{
			name: "a detected label is not yet an admission",
			in: func(t *testing.T) mqInput {
				return mqInput{
					Head:     "4f1a6643",
					Labels:   []string{mqLabel},
					Activity: mqActivityPending,
					Events: []mqLabelEvent{
						{At: mqAt(t, "2026-08-27T10:58:00Z"), Name: mqLabel, Actor: "yasyf", Added: true},
					},
				}
			},
			phase: mqPhasePending,
			label: mqLabel,
			held:  "4f1a6643",
		},
		{
			name: "a label nobody has acknowledged is only a label",
			in: func(t *testing.T) mqInput {
				return mqInput{
					Head:   "9873c9b0",
					Labels: []string{mqLabel},
					Events: []mqLabelEvent{
						{At: mqAt(t, "2026-08-27T13:42:27Z"), Name: mqLabel, Actor: "yasyf", Added: true},
					},
					Commits: []mqCommit{{OID: "9873c9b0", At: mqAt(t, "2026-08-27T13:40:09Z")}},
				}
			},
			phase: mqPhaseLabeled,
			label: mqLabel,
			held:  "9873c9b0",
		},
		{
			name: "a label a human took back off is a withdrawal",
			in: func(t *testing.T) mqInput {
				return mqInput{
					Head: "9873c9b0",
					Events: []mqLabelEvent{
						{At: mqAt(t, "2026-08-27T13:42:27Z"), Name: mqLabel, Actor: "yasyf", Added: true},
						{At: mqAt(t, "2026-08-27T13:50:00Z"), Name: mqLabel, Actor: "yasyf"},
					},
				}
			},
			phase: mqPhaseWithdrawn,
			label: mqLabel,
		},
		{
			name: "a pull request the queue never saw has no queue",
			in: func(t *testing.T) mqInput {
				return mqInput{
					Head:   "a612ebf8",
					Labels: []string{"automation", "docs"},
					Events: []mqLabelEvent{
						{At: mqAt(t, "2026-08-27T09:05:23Z"), Name: "automation", Actor: "github-actions", Added: true},
					},
				}
			},
			nilOut: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mergeQueue(tt.in(t))
			if tt.nilOut {
				if got != nil {
					t.Fatalf("mergeQueue() = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("mergeQueue() = nil, want a report")
			}
			if got.Phase != tt.phase {
				t.Errorf("Phase = %q, want %q", got.Phase, tt.phase)
			}
			if got.Label != tt.label {
				t.Errorf("Label = %q, want %q", got.Label, tt.label)
			}
			if got.DraftPR != tt.draft {
				t.Errorf("DraftPR = %d, want %d", got.DraftPR, tt.draft)
			}
			if got.Held != tt.held {
				t.Errorf("Held = %q, want %q", got.Held, tt.held)
			}
			dropped := make([]string, 0, len(got.Dropped))
			for _, c := range got.Dropped {
				dropped = append(dropped, c.OID)
			}
			if len(dropped) != len(tt.dropped) {
				t.Fatalf("Dropped = %q, want %q", dropped, tt.dropped)
			}
			for i, oid := range dropped {
				if oid != tt.dropped[i] {
					t.Errorf("Dropped[%d] = %q, want %q", i, oid, tt.dropped[i])
				}
			}
			if drifted := len(tt.dropped) > 0 && tt.held != ""; got.Drifted() != drifted {
				t.Errorf("Drifted() = %v, want %v", got.Drifted(), drifted)
			}
		})
	}
}

// TestMQValueNamesTheDrift pins the one line a reader has to act on, and the
// detail it quotes back — Graphite's own sentence, stripped of its markdown.
func TestMQValueNamesTheDrift(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		q    statusQueue
		head string
		want string
	}{
		{
			name: "drift names both shas and the count",
			q: statusQueue{
				Phase:   mqPhaseQueued,
				Label:   mqLabelFast,
				Dropped: []statusCommit{{OID: "8259d97ba"}},
				Held:    "861f15e6c",
			},
			head: "8259d97ba",
			want: "queued as merge-fast · holding 861f15e6, head is 8259d97b · 1 commit would not land",
		},
		{
			name: "no drift says so once",
			q:    statusQueue{Phase: mqPhaseQueued, Label: mqLabel, DraftPR: 16565, Held: "b430aabbc"},
			head: "b430aabbc",
			want: "queued as merge · draft #16565 · holding the branch head",
		},
		{
			name: "a rejection quotes the queue's reason",
			q: statusQueue{
				Phase:  mqPhaseRejected,
				Detail: "The Graphite merge queue removed this pull request due to downstack failures on PR #16372",
			},
			head: "4f1a6643",
			want: "rejected · The Graphite merge queue removed this pull request due to downstack failures on PR #16372",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := mqValue(tt.q, tt.head); got != tt.want {
				t.Errorf("mqValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMQDraftNumbers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		activity string
		want     []int
	}{
		{"every draft, deduplicated, in order", mqActivityQueued, []int{16560, 16565}},
		{"a rejection names no draft of its own", mqActivityRejected, []int{16562}},
		{"a pending label names none", mqActivityPending, nil},
		{"no comment names none", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mqDraftNumbers(tt.activity)
			if len(got) != len(tt.want) {
				t.Fatalf("mqDraftNumbers() = %v, want %v", got, tt.want)
			}
			for i, n := range got {
				if n != tt.want[i] {
					t.Errorf("mqDraftNumbers()[%d] = %d, want %d", i, n, tt.want[i])
				}
			}
		})
	}
}
