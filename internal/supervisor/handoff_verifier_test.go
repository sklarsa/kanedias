package supervisor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

type recordedGitCall struct {
	name string
	args []string
}

func TestGitHubHandoffVerifierUsesTrustedAllowlistedRemoteAndExactTip(t *testing.T) {
	var calls []recordedGitCall
	verifier, err := NewGitHubHandoffVerifier([]string{"owner/one", "other/two"}, func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, recordedGitCall{name: name, args: append([]string(nil), args...)})
		ref := args[len(args)-1]
		head := strings.Repeat("a", 40)
		if strings.Contains(ref, "two") { // branch names below distinguish the calls.
			head = strings.Repeat("b", 40)
		}
		if ref == "refs/heads/feature-two" {
			head = strings.Repeat("b", 40)
		}
		return []byte(head + "\t" + ref + "\n"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request := []contract.RepositoryHandoff{
		{Repository: "owner/one", BaseCommit: strings.Repeat("0", 40), Branch: "feature-one", HeadCommit: strings.Repeat("a", 40)},
		{Repository: "other/two", BaseCommit: strings.Repeat("1", 40), Branch: "feature-two", HeadCommit: strings.Repeat("b", 40)},
	}
	if err := verifier.Verify(context.Background(), request); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	want := []recordedGitCall{
		{name: "git", args: []string{"ls-remote", "--exit-code", "--refs", "https://github.com/owner/one.git", "refs/heads/feature-one"}},
		{name: "git", args: []string{"ls-remote", "--exit-code", "--refs", "https://github.com/other/two.git", "refs/heads/feature-two"}},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("git calls = %#v, want %#v", calls, want)
	}
}

func TestGitHubHandoffVerifierRejectsForgedInputsAndTimeout(t *testing.T) {
	head := strings.Repeat("a", 40)
	cases := []struct {
		name string
		repo contract.RepositoryHandoff
	}{
		{name: "not allowlisted", repo: contract.RepositoryHandoff{Repository: "evil/repo", BaseCommit: head, Branch: "feature", HeadCommit: head}},
		{name: "invalid branch", repo: contract.RepositoryHandoff{Repository: "owner/repo", BaseCommit: head, Branch: "../main", HeadCommit: head}},
		{name: "invalid head", repo: contract.RepositoryHandoff{Repository: "owner/repo", BaseCommit: head, Branch: "feature", HeadCommit: "HEAD"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			verifier, err := NewGitHubHandoffVerifier([]string{"owner/repo"}, func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("git must not run for invalid input")
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := verifier.Verify(context.Background(), []contract.RepositoryHandoff{test.repo}); err == nil {
				t.Fatal("Verify() succeeded")
			}
		})
	}

	verifier, err := NewGitHubHandoffVerifier([]string{"owner/repo"}, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	verifier.timeout = time.Millisecond
	err = verifier.Verify(context.Background(), []contract.RepositoryHandoff{{Repository: "owner/repo", BaseCommit: head, Branch: "feature", HeadCommit: head}})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Verify() error = %v, want deadline exceeded", err)
	}
}

func TestGitHubHandoffVerifierRejectsMissingMismatchAndDuplicateRemoteRecords(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, output := range []string{"", strings.Repeat("b", 40) + "\trefs/heads/feature\n", head + "\trefs/heads/feature\n" + head + "\trefs/heads/feature\n"} {
		verifier, err := NewGitHubHandoffVerifier([]string{"owner/repo"}, func(context.Context, string, ...string) ([]byte, error) {
			return []byte(output), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := verifier.Verify(context.Background(), []contract.RepositoryHandoff{{Repository: "owner/repo", BaseCommit: head, Branch: "feature", HeadCommit: head}}); err == nil {
			t.Fatalf("Verify() accepted output %q", output)
		}
	}
}
