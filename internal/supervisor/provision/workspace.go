package provision

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

const (
	managedUser = "kanedias"
	managedHome = "/home/kanedias"
)

type instanceExecClient interface {
	Exec(context.Context, string, incusclient.ExecRequest) (string, string, error)
}

// prepareSessionWorkspace enforces durable ownership and mode on a cloned
// workspace's mounted seed, then validates an explicitly selected checkout.
func prepareSessionWorkspace(ctx context.Context, client instanceExecClient, instance string, start config.WorkspaceStart) error {
	commands := [][]string{
		{"chown", managedUser + ":" + managedUser, config.WorkspaceRoot},
		{"chmod", "0755", config.WorkspaceRoot},
		{"install", "-d", "-o", managedUser, "-g", managedUser, "-m", "0755", config.WorkspaceRepositoriesRoot},
		{"chown", managedUser + ":" + managedUser, config.WorkspaceRepositoriesRoot},
		{"chmod", "0755", config.WorkspaceRepositoriesRoot},
	}
	for _, command := range commands {
		if _, _, err := client.Exec(ctx, instance, incusclient.ExecRequest{Command: command}); err != nil {
			return fmt.Errorf("prepare session workspace: execute %q: %w", strings.Join(command, " "), err)
		}
	}

	if start.Repository == "" {
		return nil
	}
	expected := start.Directory()
	checks := [][]string{
		{"test", "!", "-L", expected},
		{"test", "-d", expected},
		{"test", "!", "-L", filepath.Join(expected, ".git")},
		{"test", "-d", filepath.Join(expected, ".git")},
		{"realpath", "-e", "--", expected},
		managedGitCommand("git", "-C", expected, "rev-parse", "--show-toplevel"),
	}
	for index, command := range checks {
		stdout, stderr, err := client.Exec(ctx, instance, incusclient.ExecRequest{Command: command})
		if err != nil {
			underlying := fmt.Errorf("validate selected workspace repository: execute %q: %w", strings.Join(command, " "), err)
			if trimmed := strings.TrimSpace(stderr); trimmed != "" {
				underlying = fmt.Errorf("%w: %s", underlying, trimmed)
			}
			return unavailableWorkspaceRepository(underlying)
		}
		if index >= 4 && strings.TrimSpace(stdout) != expected {
			return unavailableWorkspaceRepository(fmt.Errorf("validate selected workspace repository: %q got %q, want %q", strings.Join(command, " "), strings.TrimSpace(stdout), expected))
		}
	}
	return nil
}

func managedGitCommand(command ...string) []string {
	prefix := []string{
		"runuser", "-u", managedUser, "--",
		"env", "HOME=" + managedHome, "USER=" + managedUser, "LOGNAME=" + managedUser,
	}
	return append(prefix, command...)
}

func unavailableWorkspaceRepository(underlying error) error {
	return errors.Join(
		contract.NewError(contract.ErrorWorkspaceRepositoryUnavailable, "selected workspace repository is unavailable"),
		underlying,
	)
}
