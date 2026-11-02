package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-notes/notes"

	"github.com/yasyf/cc-context/internal/semsearch/embed"
	"github.com/yasyf/cc-context/internal/semsearch/engine"
	"github.com/yasyf/cc-context/internal/semsearch/index"
)

type answerMatchQuery struct {
	Type    string   `json:"type"`
	Query   *string  `json:"query"`
	Exclude []string `json:"exclude"`
}

type answerMatchAnswer struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

type answerMatchResult struct {
	Type    string              `json:"type"`
	Answers []answerMatchAnswer `json:"answers"`
}

func newMatchAnswersCmd() *cobra.Command {
	var repo, program, branch, directory string
	var k int
	var session bool
	cmd := &cobra.Command{
		Use:   "match-answers",
		Short: "Rank canonical answers from one held source cut",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !session {
				return fmt.Errorf("match-answers requires --json-session")
			}
			return runAnswerMatch(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), repo, notes.AnswerCorpusFilter{Program: program, Branch: branch, CacheDir: directory}, k)
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "source repository")
	cmd.Flags().StringVar(&program, "program", "", "program label")
	cmd.Flags().StringVar(&branch, "branch", "", "additional branch anchor")
	cmd.Flags().StringVar(&directory, "corpus-dir", "", "native-owned corpus cache root")
	cmd.Flags().IntVarP(&k, "k", "k", 5, "maximum ranked chunks before deduplication")
	cmd.Flags().BoolVar(&session, "json-session", false, "exchange one ready/query/result JSON session")
	for _, name := range []string{"repo", "program", "corpus-dir"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

func runAnswerMatch(ctx context.Context, input io.Reader, output io.Writer, repo string, filter notes.AnswerCorpusFilter, k int) (err error) {
	client, err := notes.Open(repo)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	cut, err := client.OpenAnswerCorpus(ctx, filter)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, cut.Close()) }()
	return answerMatchSession(ctx, input, output, cut, filter.CacheDir, k, rankAnswerMatch)
}

func answerMatchSession(ctx context.Context, input io.Reader, output io.Writer, cut *notes.AnswerCorpusCut, root string, k int, search func(context.Context, index.OwnedAnswerCorpus, string, string, []string, int) ([]string, error)) error {
	encoder := json.NewEncoder(output)
	if err := encoder.Encode(struct {
		Type     string `json:"type"`
		Protocol int    `json:"protocol"`
	}{Type: "ready", Protocol: 1}); err != nil {
		return err
	}
	data, err := bufio.NewReader(input).ReadBytes('\n')
	if errors.Is(err, io.EOF) && len(data) == 0 {
		return nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("match-answers query frame is not UTF-8")
	}
	var request answerMatchQuery
	if err := json.Unmarshal(data, &request); err != nil {
		return fmt.Errorf("decode match-answers query: %w", err)
	}
	if request.Type != "query" || request.Query == nil {
		return fmt.Errorf("match-answers requires a query frame")
	}
	excluded := map[string]bool{}
	for _, id := range request.Exclude {
		excluded[id] = true
	}
	corpus := index.OwnedAnswerCorpus{Binding: cut.Binding, Generation: cut.Generation, Source: cut}
	candidate := false
	for _, record := range cut.Records {
		corpus.Documents = append(corpus.Documents, index.OwnedAnswerDocument{ID: record.ShortID, Path: record.Path, TextDigest: record.TextDigest})
		if !excluded[record.ShortID] {
			candidate = true
		}
	}
	result := answerMatchResult{Type: "result", Answers: []answerMatchAnswer{}}
	if !candidate || strings.TrimSpace(*request.Query) == "" {
		return encoder.Encode(result)
	}
	ids, err := search(ctx, corpus, root, *request.Query, request.Exclude, k)
	if err != nil {
		return err
	}
	answers, err := cut.Pick(ctx, ids)
	if err != nil {
		return err
	}
	for _, answer := range answers {
		result.Answers = append(result.Answers, answerMatchAnswer{ID: string(answer.ID), Title: answer.Title, Body: answer.Body})
	}
	return encoder.Encode(result)
}

func rankAnswerMatch(ctx context.Context, corpus index.OwnedAnswerCorpus, root, query string, excluded []string, k int) (ids []string, err error) {
	emb, err := embed.New(ctx, embed.CodePin)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, emb.Close(ctx)) }()
	return engine.MatchAnswers(ctx, emb, corpus, root, query, excluded, k)
}
