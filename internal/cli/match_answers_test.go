package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yasyf/cc-notes/notes"

	"github.com/yasyf/cc-context/internal/semsearch/index"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestAnswerMatchFramesBeforeSearch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		invalid bool
	}{
		{name: "eof"},
		{name: "partial", input: `{"type":"query"`, invalid: true},
		{name: "wrong type", input: `{"type":"other","query":"x"}`, invalid: true},
		{name: "missing query", input: `{"type":"query"}`, invalid: true},
		{name: "invalid UTF-8", input: "{\"type\":\"query\",\"query\":\"\xff\"}", invalid: true},
		{name: "no candidates", input: `{"type":"query","query":"alpha","exclude":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			called := false
			search := func(context.Context, index.OwnedAnswerCorpus, string, string, []string, int) ([]string, error) {
				called = true
				return nil, nil
			}
			err := answerMatchSession(t.Context(), strings.NewReader(tc.input), &output, &notes.AnswerCorpusCut{}, t.TempDir(), 5, search)
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v invalid=%v", err, tc.invalid)
			}
			if called {
				t.Fatal("search called before a candidate query")
			}
			if !strings.HasPrefix(output.String(), "{\"type\":\"ready\",\"protocol\":1}\n") {
				t.Fatalf("ready missing: %s", output.String())
			}
		})
	}
}

func TestAnswerMatchPreservesPythonPolicyInputs(t *testing.T) {
	queries := []string{"éaaaaaaaé", "αaaaaaaaβ", "\u0301aaaaaaa\u0301", "_aaaaaaa_", "1aaaaaaa2", strings.Repeat("界", 2000)}
	for _, query := range queries {
		excluded := []string{" AAAAAAA ", "aaaaaaa-extra", "12345678", "aaaaaaa"}
		input, err := json.Marshal(answerMatchQuery{Type: "query", Query: &query, Exclude: excluded})
		if err != nil {
			t.Fatal(err)
		}
		cut := &notes.AnswerCorpusCut{Binding: "binding", Generation: "generation", Records: []notes.AnswerCorpusRecord{{ShortID: "bbbbbbb", Path: "bbbbbbb.md", TextDigest: "digest"}}}
		var output bytes.Buffer
		sentinel := errors.New("rank boundary")
		called := false
		search := func(_ context.Context, corpus index.OwnedAnswerCorpus, _, got string, excludes []string, k int) ([]string, error) {
			called = true
			if got != query || fmt.Sprint(excludes) != fmt.Sprint(excluded) || k != 9 || len(corpus.Documents) != 1 {
				t.Fatalf("policy input changed: %q %v %d", got, excludes, k)
			}
			return nil, sentinel
		}
		if err := answerMatchSession(t.Context(), bytes.NewReader(input), &output, cut, t.TempDir(), 9, search); !errors.Is(err, sentinel) {
			t.Fatalf("search error=%v", err)
		}
		if !called {
			t.Fatal("search skipped")
		}
	}
}

func TestAnswerMatchSourceFailureBeforeReady(t *testing.T) {
	var output bytes.Buffer
	err := runAnswerMatch(t.Context(), strings.NewReader(""), &output, t.TempDir(), notes.AnswerCorpusFilter{Program: "p", CacheDir: t.TempDir()}, 5)
	if err == nil || output.Len() != 0 {
		t.Fatalf("source failure=%v output=%q", err, output.String())
	}
}

func TestAnswerMatchHeldCutOldWriter(t *testing.T) {
	t.Setenv("CC_NOTES_ACTOR", "Corpus Test <corpus@example.com>")
	f := vcstest.Repo(t)
	writer, err := notes.Open(f.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	answer, _, err := writer.CreateAnswer(t.Context(), notes.NoteSpec{Title: "old title", Body: "old body", Tags: []string{"scope:durable", "program:p"}})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := notes.Open(f.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	filter := notes.AnswerCorpusFilter{Program: "p", CacheDir: t.TempDir()}
	cut, err := reader.OpenAnswerCorpus(t.Context(), filter)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cut.Close() }()
	if len(cut.Records) != 1 {
		t.Fatalf("records=%v", cut.Records)
	}
	id := cut.Records[0].ShortID
	var output bytes.Buffer
	search := func(ctx context.Context, corpus index.OwnedAnswerCorpus, _, _ string, _ []string, _ int) ([]string, error) {
		text, err := corpus.Source.Text(ctx, id)
		if err != nil {
			return nil, err
		}
		if text != "# old title\n\nold body\n" {
			return nil, fmt.Errorf("old text=%q", text)
		}
		title, body := "new title", "new body"
		if _, err := writer.EditAnswer(ctx, answer.ID, notes.NoteEdit{Title: &title, Body: &body}); err != nil {
			return nil, err
		}
		return []string{id}, nil
	}
	if err := answerMatchSession(t.Context(), strings.NewReader(`{"type":"query","query":"old","exclude":[]}`), &output, cut, filter.CacheDir, 5, search); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var ready map[string]any
	var result answerMatchResult
	if err := decoder.Decode(&ready); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Answers) != 1 || result.Answers[0].ID != string(answer.ID) || result.Answers[0].Title != "old title" || result.Answers[0].Body != "old body" {
		t.Fatalf("mixed cut: %+v", result)
	}
	next, err := reader.OpenAnswerCorpus(t.Context(), filter)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Close() }()
	if next.Generation == cut.Generation {
		t.Fatal("old writer edit did not advance generation")
	}
	picked, err := next.Pick(t.Context(), []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(picked) != 1 || picked[0].Title != "new title" || picked[0].Body != "new body" {
		t.Fatalf("next cut stale: %+v", picked)
	}
}

func TestAnswerMatchNonPositiveKReachesSource(t *testing.T) {
	for _, k := range []int{0, -1} {
		t.Run(fmt.Sprint(k), func(t *testing.T) {
			var direct, output bytes.Buffer
			repo, root := t.TempDir(), t.TempDir()
			filter := notes.AnswerCorpusFilter{Program: "p", CacheDir: root}
			want := runAnswerMatch(t.Context(), strings.NewReader(""), &direct, repo, filter, k)
			cmd := newMatchAnswersCmd()
			cmd.SetIn(strings.NewReader(""))
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs([]string{"--repo", repo, "--program", "p", "--corpus-dir", root, "--json-session", "-k", fmt.Sprint(k)})
			got := cmd.ExecuteContext(t.Context())
			if want == nil || got == nil || got.Error() != want.Error() {
				t.Fatalf("source error changed for k=%d: got=%v want=%v", k, got, want)
			}
		})
	}
}
