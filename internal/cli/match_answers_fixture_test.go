package cli

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-notes/model"
	"github.com/yasyf/cc-notes/notes"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/semsearch/engine"
	"github.com/yasyf/cc-context/internal/semsearch/index"
)

type nativeMatchFixtureConfig struct {
	Operation string                      `json:"operation"`
	Repo      string                      `json:"repo"`
	Program   string                      `json:"program"`
	Branch    string                      `json:"branch"`
	Count     int                         `json:"count"`
	Metrics   string                      `json:"metrics"`
	Mirror    string                      `json:"mirror"`
	Query     string                      `json:"query"`
	K         int                         `json:"k"`
	Labels    []string                    `json:"labels"`
	Specs     []notes.NoteSpec            `json:"specs"`
	Mutation  *nativeMatchFixtureMutation `json:"mutation"`
}

type nativeMatchFixtureMutation struct {
	ID             model.EntityID `json:"id"`
	Other          model.EntityID `json:"other"`
	Tip            model.SHA      `json:"tip"`
	Action         string         `json:"action"`
	Timing         string         `json:"timing"`
	Title          *string        `json:"title"`
	Body           *string        `json:"body"`
	AddTags        []string       `json:"add_tags"`
	RemoveTags     []string       `json:"remove_tags"`
	AddBranches    []string       `json:"add_branches"`
	RemoveBranches []string       `json:"remove_branches"`
}

type nativeMatchFixtureMetrics struct {
	Operation                 string   `json:"operation"`
	SetupAnswers              int      `json:"setup_answers"`
	SetupCreateCalls          int      `json:"setup_create_calls"`
	SetupGitCalls             int      `json:"setup_git_calls"`
	SetupCanonicalReads       int      `json:"setup_canonical_reads"`
	SetupMutationCalls        int      `json:"setup_mutation_calls"`
	SetupWallNanoseconds      int64    `json:"setup_wall_nanoseconds"`
	MutationWallNanoseconds   int64    `json:"mutation_wall_nanoseconds"`
	SessionWallNanoseconds    int64    `json:"session_wall_nanoseconds"`
	Generation                string   `json:"generation,omitempty"`
	SourceRecords             int      `json:"source_records"`
	SearchCalls               int      `json:"search_calls"`
	TextCalls                 int      `json:"text_calls"`
	TextBytes                 int      `json:"text_bytes"`
	EncodeCalls               int      `json:"encode_calls"`
	EncodeTextsIncludingQuery int      `json:"encode_texts_including_query"`
	EncodeBytesIncludingQuery int      `json:"encode_bytes_including_query"`
	EncodeBatchSizes          []int    `json:"encode_batch_sizes"`
	Query                     string   `json:"query,omitempty"`
	Excluded                  []string `json:"excluded,omitempty"`
	PickRequestIDs            []string `json:"pick_request_ids"`
	ResultIDs                 []string `json:"result_ids"`
	ReadyFrames               int      `json:"ready_frames"`
	ResultFrames              int      `json:"result_frames"`
	ChunkerCalls              string   `json:"chunker_calls"`
	CanonicalRefreshRecords   string   `json:"canonical_refresh_records"`
	Error                     string   `json:"error,omitempty"`
}

type nativeMatchFixtureText struct {
	source  index.OwnedAnswerText
	metrics *nativeMatchFixtureMetrics
}

func (s nativeMatchFixtureText) Text(ctx context.Context, id string) (string, error) {
	s.metrics.TextCalls++
	text, err := s.source.Text(ctx, id)
	s.metrics.TextBytes += len(text)
	return text, err
}

type nativeMatchFixtureEmbedder struct{ metrics *nativeMatchFixtureMetrics }

func (nativeMatchFixtureEmbedder) Dims() int { return 8 }

func (e nativeMatchFixtureEmbedder) Encode(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.metrics.EncodeCalls++
	e.metrics.EncodeTextsIncludingQuery += len(texts)
	e.metrics.EncodeBatchSizes = append(e.metrics.EncodeBatchSizes, len(texts))
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		e.metrics.EncodeBytesIncludingQuery += len(text)
		digest := sha256.Sum256([]byte(text))
		vectors[i] = make([]float32, 8)
		for j := range vectors[i] {
			value := binary.LittleEndian.Uint32(digest[j*4:])
			vectors[i][j] = float32(value)/float32(1<<32) - 0.5
		}
	}
	return vectors, nil
}

type nativeMatchFixtureOutput struct{ metrics *nativeMatchFixtureMetrics }

func (w nativeMatchFixtureOutput) Write(data []byte) (int, error) {
	var frame struct {
		Type    string `json:"type"`
		Answers []struct {
			ID string `json:"id"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return 0, err
	}
	switch frame.Type {
	case "ready":
		w.metrics.ReadyFrames++
	case "result":
		w.metrics.ResultFrames++
		for _, answer := range frame.Answers {
			w.metrics.ResultIDs = append(w.metrics.ResultIDs, answer.ID)
		}
	default:
		return 0, fmt.Errorf("unexpected fixture frame %q", frame.Type)
	}
	return os.Stdout.Write(data)
}

func TestNativeAnswerMatchFixtureProcess(t *testing.T) {
	configPath := os.Getenv("CCX_NATIVE_MATCH_FIXTURE_CONFIG")
	if configPath == "" {
		return
	}
	var config nativeMatchFixtureConfig
	data, err := os.ReadFile(configPath)
	if err == nil {
		err = json.Unmarshal(data, &config)
	}
	metrics := nativeMatchFixtureMetrics{Operation: config.Operation, ChunkerCalls: "UNKNOWN", CanonicalRefreshRecords: "UNKNOWN", PickRequestIDs: []string{}, ResultIDs: []string{}, EncodeBatchSizes: []int{}}
	if err == nil {
		err = runNativeMatchFixture(context.Background(), config, &metrics)
	}
	if err != nil {
		metrics.Error = err.Error()
	}
	if config.Metrics != "" {
		encoded, encodeErr := json.Marshal(metrics)
		if encodeErr == nil {
			encodeErr = os.WriteFile(config.Metrics, append(encoded, '\n'), 0o600)
		}
		err = errors.Join(err, encodeErr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runNativeMatchFixture(ctx context.Context, config nativeMatchFixtureConfig, metrics *nativeMatchFixtureMetrics) (err error) {
	if config.Operation == "session" {
		return nativeMatchFixtureSession(ctx, config, metrics)
	}
	if config.Operation == "search" {
		start := time.Now()
		results, err := engine.Search(ctx, nativeMatchFixtureEmbedder{metrics}, backend.Args{Path: config.Mirror, Query: config.Query, K: config.K, Kind: "docs", MaxSnippetLines: 1})
		metrics.SessionWallNanoseconds = time.Since(start).Nanoseconds()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(results)
	}
	start := time.Now()
	defer func() { metrics.SetupWallNanoseconds = time.Since(start).Nanoseconds() }()
	client, err := notes.Open(config.Repo)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	switch config.Operation {
	case "seed":
		return nativeMatchFixtureSeed(ctx, client, config, metrics)
	case "create":
		answers := make([]model.Answer, 0, len(config.Specs))
		for _, spec := range config.Specs {
			answer, _, err := client.CreateAnswer(ctx, spec)
			metrics.SetupCreateCalls++
			if err != nil {
				return err
			}
			answers = append(answers, answer)
		}
		metrics.SetupAnswers = len(answers)
		return json.NewEncoder(os.Stdout).Encode(answers)
	case "mutate":
		answer, err := nativeMatchFixtureTimedMutation(ctx, client, config.Repo, config.Mutation, metrics)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(answer)
	case "answers":
		answers, err := client.Answers(ctx, notes.DocumentFilter{Labels: config.Labels, Anchors: notes.AnchorFilter{Branch: config.Branch}})
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(answers)
	default:
		return fmt.Errorf("unknown native fixture operation %q", config.Operation)
	}
}

func nativeMatchFixtureGit(ctx context.Context, repo, input string, metrics *nativeMatchFixtureMetrics, args ...string) (string, error) {
	metrics.SetupGitCalls++
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Native Match Fixture", "GIT_AUTHOR_EMAIL=fixture@example.com", "GIT_AUTHOR_DATE=1700000000 +0000", "GIT_COMMITTER_NAME=Native Match Fixture", "GIT_COMMITTER_EMAIL=fixture@example.com", "GIT_COMMITTER_DATE=1700000000 +0000")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("fixture git %v: %w", args, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func nativeMatchFixtureSeed(ctx context.Context, client *notes.Client, config nativeMatchFixtureConfig, metrics *nativeMatchFixtureMetrics) error {
	if config.Count != 32 && config.Count != 10000 {
		return fmt.Errorf("fixture count must be 32 or 10000")
	}
	ids := make([]model.EntityID, 0, config.Count)
	var updates strings.Builder
	updates.WriteString("start\n")
	for i := 0; i < config.Count; i++ {
		create := model.CreateAnswer{Nonce: fmt.Sprintf("native-python-match-fixture-%05d", i), Title: fmt.Sprintf("Ruling %d", i), Body: "Owner words λ.", Tags: []string{"scope:durable", "program:" + config.Program}}
		if config.Branch != "" && i%3 == 0 {
			create.Anchors = []model.Anchor{{Kind: model.AnchorBranch, Value: config.Branch}}
		}
		data, err := (model.Pack{Lamport: 1, Ops: []model.Op{create}}).MarshalJSON()
		if err != nil {
			return err
		}
		blob, err := nativeMatchFixtureGit(ctx, config.Repo, string(data), metrics, "hash-object", "-w", "--stdin")
		if err != nil {
			return err
		}
		tree, err := nativeMatchFixtureGit(ctx, config.Repo, "100644 blob "+blob+"\tops.json\n", metrics, "mktree")
		if err != nil {
			return err
		}
		commit, err := nativeMatchFixtureGit(ctx, config.Repo, "", metrics, "commit-tree", tree, "-m", "native answer match fixture")
		if err != nil {
			return err
		}
		ids = append(ids, model.EntityID(commit))
		fmt.Fprintf(&updates, "create refs/cc-notes/answers/%s %s\n", commit, commit)
	}
	updates.WriteString("prepare\ncommit\n")
	if _, err := nativeMatchFixtureGit(ctx, config.Repo, updates.String(), metrics, "update-ref", "--stdin"); err != nil {
		return err
	}
	answers := make([]model.Answer, 0, len(ids))
	for _, id := range ids {
		answer, err := client.Answer(ctx, id)
		metrics.SetupCanonicalReads++
		if err != nil {
			return err
		}
		answers = append(answers, answer)
	}
	metrics.SetupAnswers = len(answers)
	return json.NewEncoder(os.Stdout).Encode(answers)
}

func nativeMatchFixtureTimedMutation(ctx context.Context, client *notes.Client, repo string, mutation *nativeMatchFixtureMutation, metrics *nativeMatchFixtureMetrics) (model.Answer, error) {
	metrics.SetupMutationCalls++
	start := time.Now()
	defer func() { metrics.MutationWallNanoseconds += time.Since(start).Nanoseconds() }()
	if mutation != nil && mutation.Action == "delete_ref" {
		answer, err := client.Answer(ctx, mutation.ID)
		metrics.SetupCanonicalReads++
		if err != nil {
			return model.Answer{}, err
		}
		command := fmt.Sprintf("delete refs/cc-notes/answers/%s %s\n", answer.ID, answer.Head)
		_, err = nativeMatchFixtureGit(ctx, repo, command, metrics, "update-ref", "--stdin")
		return answer, err
	}
	if mutation != nil && mutation.Action == "restore_ref" {
		command := fmt.Sprintf("create refs/cc-notes/answers/%s %s\n", mutation.ID, mutation.Tip)
		if _, err := nativeMatchFixtureGit(ctx, repo, command, metrics, "update-ref", "--stdin"); err != nil {
			return model.Answer{}, err
		}
		answer, err := client.Answer(ctx, mutation.ID)
		metrics.SetupCanonicalReads++
		return answer, err
	}
	return nativeMatchFixtureMutate(ctx, client, mutation)
}

func nativeMatchFixtureMutate(ctx context.Context, client *notes.Client, mutation *nativeMatchFixtureMutation) (model.Answer, error) {
	if mutation == nil {
		return model.Answer{}, fmt.Errorf("fixture mutation required")
	}
	switch mutation.Action {
	case "edit":
		return client.EditAnswer(ctx, mutation.ID, notes.NoteEdit{Title: mutation.Title, Body: mutation.Body, AddTags: mutation.AddTags, RemoveTags: mutation.RemoveTags, AddAnchors: notes.AnchorSpec{Branches: mutation.AddBranches}, RemoveAnchors: notes.AnchorSpec{Branches: mutation.RemoveBranches}})
	case "delete":
		return client.RemoveAnswer(ctx, mutation.ID)
	case "supersede":
		return client.SupersedeAnswer(ctx, mutation.ID, mutation.Other)
	case "unsupersede":
		return client.UnsupersedeAnswer(ctx, mutation.ID, mutation.Other)
	default:
		return model.Answer{}, fmt.Errorf("unknown fixture mutation %q", mutation.Action)
	}
}

func nativeMatchFixtureSession(ctx context.Context, config nativeMatchFixtureConfig, metrics *nativeMatchFixtureMetrics) (err error) {
	start := time.Now()
	defer func() { metrics.SessionWallNanoseconds = time.Since(start).Nanoseconds() }()
	if config.Mutation != nil && config.Mutation.Timing != "before_rank" && config.Mutation.Timing != "after_rank" {
		return fmt.Errorf("fixture mutation timing must be before_rank or after_rank")
	}
	args := flag.Args()
	if len(args) < 2 || args[0] != "code" || args[1] != "match-answers" {
		return fmt.Errorf("fixture requires native match-answers arguments after --")
	}
	cmd := newMatchAnswersCmd()
	if err := cmd.ParseFlags(args[2:]); err != nil {
		return err
	}
	getString := func(name string) string {
		value, getErr := cmd.Flags().GetString(name)
		if getErr != nil {
			panic(getErr)
		}
		return value
	}
	k, err := cmd.Flags().GetInt("k")
	if err != nil {
		return err
	}
	repo, program, branch, root := getString("repo"), getString("program"), getString("branch"), getString("corpus-dir")
	var writer *notes.Client
	if config.Mutation != nil {
		writer, err = notes.Open(repo)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, writer.Close()) }()
	}
	client, err := notes.Open(repo)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	cut, err := client.OpenAnswerCorpus(ctx, notes.AnswerCorpusFilter{Program: program, Branch: branch, CacheDir: root})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, cut.Close()) }()
	metrics.Generation = cut.Generation
	metrics.SourceRecords = len(cut.Records)
	search := func(ctx context.Context, corpus index.OwnedAnswerCorpus, root, query string, excluded []string, k int) ([]string, error) {
		metrics.SearchCalls++
		metrics.Query = query
		metrics.Excluded = append([]string(nil), excluded...)
		if config.Mutation != nil && config.Mutation.Timing == "before_rank" {
			if _, err := nativeMatchFixtureTimedMutation(ctx, writer, repo, config.Mutation, metrics); err != nil {
				return nil, err
			}
		}
		corpus.Source = nativeMatchFixtureText{source: corpus.Source, metrics: metrics}
		ids, err := engine.MatchAnswers(ctx, nativeMatchFixtureEmbedder{metrics}, corpus, root, query, excluded, k)
		if err != nil {
			return nil, err
		}
		metrics.PickRequestIDs = append(metrics.PickRequestIDs, ids...)
		if config.Mutation != nil && config.Mutation.Timing == "after_rank" {
			if _, err := nativeMatchFixtureTimedMutation(ctx, writer, repo, config.Mutation, metrics); err != nil {
				return nil, err
			}
		}
		return ids, nil
	}
	return answerMatchSession(ctx, os.Stdin, nativeMatchFixtureOutput{metrics}, cut, root, k, search)
}
