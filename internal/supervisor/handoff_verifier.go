package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

const defaultHandoffVerificationTimeout = 15 * time.Second

var (
	githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,98}[A-Za-z0-9])?/[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,98}[A-Za-z0-9])?$`)
	gitObjectIDPattern      = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)
	gitBranchPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

type HandoffVerifier interface {
	Verify(context.Context, []contract.RepositoryHandoff) error
}

type HandoffVerifierFunc func(context.Context, []contract.RepositoryHandoff) error

func (verify HandoffVerifierFunc) Verify(ctx context.Context, repositories []contract.RepositoryHandoff) error {
	return verify(ctx, repositories)
}

type GitCommandRunner func(context.Context, string, ...string) ([]byte, error)

type GitHubHandoffVerifier struct {
	remotes map[string]string
	run     GitCommandRunner
	timeout time.Duration
}

func NewGitHubHandoffVerifier(repositories []string, runner GitCommandRunner) (*GitHubHandoffVerifier, error) {
	remotes := make(map[string]string, len(repositories))
	for _, repository := range repositories {
		if !githubRepositoryPattern.MatchString(repository) {
			return nil, fmt.Errorf("trusted workspace repository %q is not an owner/repository slug", repository)
		}
		if _, duplicate := remotes[repository]; duplicate {
			return nil, fmt.Errorf("trusted workspace repository %q is duplicated", repository)
		}
		remotes[repository] = "https://github.com/" + repository + ".git"
	}
	if runner == nil {
		runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		}
	}
	return &GitHubHandoffVerifier{remotes: remotes, run: runner, timeout: defaultHandoffVerificationTimeout}, nil
}

func (verifier *GitHubHandoffVerifier) Verify(ctx context.Context, repositories []contract.RepositoryHandoff) error {
	if verifier == nil {
		return contract.NewError(contract.ErrorInternal, "host GitHub handoff verifier is unavailable")
	}
	seen := make(map[string]struct{}, len(repositories))
	for _, repository := range repositories {
		remote, allowed := verifier.remotes[repository.Repository]
		if !allowed {
			return contract.NewError(contract.ErrorHandoffRefMissing, fmt.Sprintf("repository %q is not in trusted workspace.repos", repository.Repository))
		}
		if _, duplicate := seen[repository.Repository]; duplicate {
			return contract.NewError(contract.ErrorInvalidRequest, "handoff repositories must be unique")
		}
		seen[repository.Repository] = struct{}{}
		if !validBranch(repository.Branch) {
			return contract.NewError(contract.ErrorInvalidRequest, fmt.Sprintf("handoff branch %q is invalid", repository.Branch))
		}
		if !gitObjectIDPattern.MatchString(repository.BaseCommit) || !gitObjectIDPattern.MatchString(repository.HeadCommit) {
			return contract.NewError(contract.ErrorInvalidRequest, "handoff baseCommit and headCommit must be lowercase full Git object IDs")
		}

		verifyCtx, cancel := context.WithTimeout(ctx, verifier.timeout)
		ref := "refs/heads/" + repository.Branch
		output, err := verifier.run(verifyCtx, "git", "ls-remote", "--exit-code", "--refs", remote, ref)
		contextErr := verifyCtx.Err()
		cancel()
		if err != nil {
			if contextErr != nil {
				return errors.Join(contract.NewError(contract.ErrorChildUnavailable, fmt.Sprintf("trusted GitHub verification timed out for %s:%s", repository.Repository, repository.Branch)), contextErr)
			}
			return errors.Join(contract.NewError(contract.ErrorHandoffRefMissing, fmt.Sprintf("trusted GitHub ref is unavailable for %s:%s", repository.Repository, repository.Branch)), err)
		}
		matches := 0
		observed := ""
		scanner := bufio.NewScanner(strings.NewReader(string(output)))
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) == 2 && fields[1] == ref {
				matches++
				observed = fields[0]
			}
		}
		if err := scanner.Err(); err != nil {
			return contract.NewError(contract.ErrorChildUnavailable, "parse trusted GitHub response: "+err.Error())
		}
		if matches != 1 {
			return contract.NewError(contract.ErrorHandoffRefMissing, fmt.Sprintf("trusted GitHub ref %s:%s returned %d exact records", repository.Repository, repository.Branch, matches))
		}
		if observed != repository.HeadCommit {
			return contract.NewError(contract.ErrorHandoffRefMismatch, fmt.Sprintf("trusted GitHub ref %s:%s is %s, expected %s", repository.Repository, repository.Branch, observed, repository.HeadCommit))
		}
	}
	return nil
}

func validBranch(branch string) bool {
	if len(branch) > 255 || !gitBranchPattern.MatchString(branch) || branch == "@" || strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.Contains(branch, "@{") || strings.ContainsAny(branch, " ~^:?*[\\\x7f") {
		return false
	}
	for _, component := range strings.Split(branch, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
		for _, character := range component {
			if character < 0x20 {
				return false
			}
		}
	}
	return true
}
