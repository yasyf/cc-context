package nativeapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/execstub"
)

func fakeTrunkAWS(t *testing.T, script string) (TrunkSSMConfig, string) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	execstub.Write(t, filepath.Join(dir, "aws"), "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$CCX_FAKE_AWS_CALLS\"\n"+script+"\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CCX_FAKE_AWS_CALLS", calls)
	t.Setenv("TRUNK_MERGE_API_KEY", "")
	return TrunkSSMConfig{SSMParameter: "/ccx/" + t.Name(), AWSProfile: "developer", AWSRegion: "us-east-2"}, calls
}

func TestTrunkSSMEnvironmentWinsBeforeInvalidConfig(t *testing.T) {
	_, calls := fakeTrunkAWS(t, "exit 99")
	t.Setenv("TRUNK_MERGE_API_KEY", "explicit-key")
	client := New("http://unused").WithTrunkSSM(TrunkSSMConfig{SSMParameter: "incomplete"})
	token, err := client.trunkToken(context.Background())
	if err != nil || token != "explicit-key" {
		t.Fatalf("credential selection failed: %v", err)
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("AWS unexpectedly executed: %v", err)
	}
}

func TestTrunkSSMLazyLookupCachesAcrossClients(t *testing.T) {
	config, calls := fakeTrunkAWS(t, "printf 'ssm-fixture-key\\n'")
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer ssm-fixture-key" {
			t.Error("request did not use the resolved SSM credential")
		}
		if _, err := fmt.Fprint(w, `{}`); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	for range 2 {
		client := New("http://unused").WithTrunkSSM(config).WithQueueProvider("trunk")
		client.queueBase = srv.URL
		if err := client.queueGet(context.Background(), "/v2/probe", nil, &map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	args, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ssm", "get-parameter", "--name", config.SSMParameter, "--with-decryption", "--profile", config.AWSProfile, "--region", config.AWSRegion, "--query", "Parameter.Value", "--output", "text"}
	if requests != 2 || !reflect.DeepEqual(strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"), want) {
		t.Fatalf("requests=%d AWS argv=%q", requests, args)
	}
	t.Setenv("TRUNK_MERGE_API_KEY", "new-explicit-key")
	token, err := New("http://unused").WithTrunkSSM(config).trunkToken(context.Background())
	if err != nil || token != "new-explicit-key" {
		t.Fatalf("explicit credential did not override cached SSM: %v", err)
	}
}

func TestTrunkSSMIsUnusedByGitHubMetadataAndQueue(t *testing.T) {
	config, calls := fakeTrunkAWS(t, "exit 99")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, `{"data":{"repository":{"p0":{"number":1,"state":"OPEN","mergeQueueEntry":null}}}}`); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	client := NewWithToken(srv.URL, "github-key").WithTrunkSSM(config)
	if _, err := client.PullRequestInfo(context.Background(), PullRequestInfoRequest{RepoOwner: "org", RepoName: "repo", PRNumbers: []int{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.MergeabilityStatuses(context.Background(), "org", "repo", []int{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-Trunk requests executed AWS: %v", err)
	}
}

func TestTrunkSSMFailureDoesNotExposeCapturedOutput(t *testing.T) {
	config, calls := fakeTrunkAWS(t, "printf 'secret-stdout\\n'; printf 'secret-stderr\\n' >&2; exit 42")
	client := New("http://unused").WithTrunkSSM(config)
	for range 2 {
		_, err := client.trunkToken(context.Background())
		if err == nil || !strings.Contains(err.Error(), "exit code 42") || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe AWS diagnostic: %v", err)
		}
	}
	args, err := os.ReadFile(calls)
	if err != nil || strings.Count(string(args), "get-parameter\n") != 2 {
		t.Fatalf("failure was cached: %v", err)
	}
}

func TestTrunkSSMRejectsMissingAndMalformedConfiguration(t *testing.T) {
	_, calls := fakeTrunkAWS(t, "exit 99")
	for _, config := range []TrunkSSMConfig{
		{},
		{SSMParameter: "/ccx/token"},
		{SSMParameter: "/ccx/token", AWSProfile: "developer"},
		{SSMParameter: "/ccx/token", AWSProfile: "developer\nsecret", AWSRegion: "us-east-2"},
		{SSMParameter: "/ccx/bad name", AWSProfile: "developer", AWSRegion: "us-east-2"},
		{SSMParameter: "/ccx/token", AWSProfile: "developer", AWSRegion: "--debug"},
	} {
		_, err := New("http://unused").WithTrunkSSM(config).trunkToken(context.Background())
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid configuration accepted or exposed: %v", err)
		}
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid configuration executed AWS: %v", err)
	}
}

func TestTrunkSSMRejectsEmptyAndMultilineValues(t *testing.T) {
	for _, script := range []string{"printf '\\n'", "printf 'None\\n'", "printf 'first-secret\\nsecond-secret\\n'"} {
		t.Run(script, func(t *testing.T) {
			config, _ := fakeTrunkAWS(t, script)
			_, err := New("http://unused").WithTrunkSSM(config).trunkToken(context.Background())
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid value accepted or exposed: %v", err)
			}
		})
	}
}

func TestTrunkSSMRespectsCancellationBeforeExecution(t *testing.T) {
	config, calls := fakeTrunkAWS(t, "exit 99")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New("http://unused").WithTrunkSSM(config).trunkToken(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved: %v", err)
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled resolution executed AWS: %v", err)
	}
}

func TestTrunkSSMCancelsRunningAWS(t *testing.T) {
	config, _ := fakeTrunkAWS(t, "exec /bin/sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := New("http://unused").WithTrunkSSM(config).trunkToken(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("running AWS cancellation not preserved: %v", err)
	}
}

func TestTrunkSSMCancelsWaitingResolver(t *testing.T) {
	config, calls := fakeTrunkAWS(t, "exit 99")
	client := New("http://unused").WithTrunkSSM(config)
	client.trunkSSM.gate <- struct{}{}
	defer func() { <-client.trunkSSM.gate }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := client.trunkToken(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting resolver cancellation not preserved: %v", err)
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("waiting resolver executed AWS: %v", err)
	}
}
