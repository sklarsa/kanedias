package workspace

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
)

type repository struct {
	slug string
	name string
	url  string
}

func parseRepositories(slugs []string) ([]repository, error) {
	configured, err := config.ParseWorkspaceRepositories(slugs)
	if err != nil {
		return nil, err
	}
	repositories := make([]repository, 0, len(configured))
	for _, item := range configured {
		repositories = append(repositories, repository{
			slug: item.Slug,
			name: item.Checkout,
			url:  "https://github.com/" + item.Slug + ".git",
		})
	}
	return repositories, nil
}

var managedCommandPrefix = []string{
	"runuser", "-u", managedUser, "--",
	"env", "HOME=" + managedHome, "USER=" + managedUser, "LOGNAME=" + managedUser,
}

func prepareWorkspaceRoot(ctx context.Context, incus client, instance string, stdout, stderr io.Writer) error {
	if err := exec(ctx, incus, instance, stdout, stderr, []string{"test", "!", "-L", workspacePath + "/repos"}); err != nil {
		return fmt.Errorf("refusing symlinked repository root: %s/repos: %w", workspacePath, err)
	}
	// Enforce durable ownership and mode with explicit root argument arrays so an
	// existing (already-mounted) seeded directory is repaired, not only created.
	commands := [][]string{
		{"chown", managedUser + ":" + managedUser, workspacePath},
		{"chmod", "0755", workspacePath},
		{"install", "-d", "-o", managedUser, "-g", managedUser, "-m", "0755", workspacePath + "/repos"},
		{"chown", managedUser + ":" + managedUser, workspacePath + "/repos"},
		{"chmod", "0755", workspacePath + "/repos"},
	}
	for _, command := range commands {
		if err := exec(ctx, incus, instance, stdout, stderr, command); err != nil {
			return err
		}
	}
	return nil
}

func syncRepositories(ctx context.Context, incus client, instance string, repositories []repository, stdout, stderr io.Writer) error {
	setupCommands := [][]string{
		{"gh", "auth", "setup-git", "--hostname", "github.com", "--force"},
		{"git", "config", "--global", "--replace-all", "url.https://github.com/.insteadOf", "git@github.com:"},
		{"git", "config", "--global", "--add", "url.https://github.com/.insteadOf", "ssh://git@github.com/"},
	}
	for _, command := range setupCommands {
		if err := execManaged(ctx, incus, instance, stdout, stderr, command); err != nil {
			return err
		}
	}

	for _, repository := range repositories {
		_, _ = fmt.Fprintf(stdout, "Syncing %s...\n", repository.slug)
		if err := syncRepository(ctx, incus, instance, repository, stdout, stderr); err != nil {
			return err
		}
	}
	return nil
}

func syncRepository(ctx context.Context, incus client, instance string, repository repository, stdout, stderr io.Writer) error {
	target := workspacePath + "/repos/" + repository.name
	_, _, existsErr := incus.Exec(ctx, instance, incusclient.ExecRequest{
		Command: managedCommand([]string{"test", "-e", target}),
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if existsErr != nil {
		if err := execManaged(ctx, incus, instance, stdout, stderr, []string{
			"gh", "repo", "clone", repository.url, target, "--", "--recurse-submodules",
		}); err != nil {
			return err
		}
	} else {
		checks := []struct {
			command []string
			message string
		}{
			{[]string{"test", "!", "-L", target}, "refusing symlinked repository path"},
			{[]string{"test", "-d", target + "/.git"}, "existing path is not a self-contained Git repository"},
			{[]string{"test", "!", "-L", target + "/.git"}, "existing path is not a self-contained Git repository"},
		}
		for _, check := range checks {
			if err := execManaged(ctx, incus, instance, stdout, stderr, check.command); err != nil {
				return fmt.Errorf("%s: %s: %w", check.message, target, err)
			}
		}
		worktree, err := execManagedOutput(ctx, incus, instance, stdout, stderr, []string{
			"git", "-C", target, "rev-parse", "--show-toplevel",
		})
		if err != nil {
			return err
		}
		if strings.TrimSpace(worktree) != target {
			return fmt.Errorf("repository worktree escapes its expected path: %s", target)
		}
	}

	commands := [][]string{
		{"git", "-C", target, "remote", "set-url", "origin", repository.url},
		{"git", "-C", target, "fetch", "--force", "--prune", "--prune-tags", "--tags", "origin"},
		{"git", "-C", target, "remote", "set-head", "origin", "--auto"},
	}
	for _, command := range commands {
		if err := execManaged(ctx, incus, instance, stdout, stderr, command); err != nil {
			return err
		}
	}

	defaultRef, err := execManagedOutput(ctx, incus, instance, stdout, stderr, []string{
		"git", "-C", target, "symbolic-ref", "refs/remotes/origin/HEAD",
	})
	if err != nil {
		return err
	}
	defaultRef = strings.TrimSpace(defaultRef)
	const remotePrefix = "refs/remotes/origin/"
	if !strings.HasPrefix(defaultRef, remotePrefix) || defaultRef == remotePrefix {
		return fmt.Errorf("repository %s returned invalid default remote ref %q", repository.slug, defaultRef)
	}
	branch := strings.TrimPrefix(defaultRef, remotePrefix)

	commands = [][]string{
		{"git", "-C", target, "checkout", "--force", "-B", branch, defaultRef},
		{"git", "-C", target, "reset", "--hard", defaultRef},
		{"git", "-C", target, "clean", "-ffdx"},
		{"git", "-C", target, "submodule", "sync", "--recursive"},
		{"git", "-C", target, "submodule", "update", "--init", "--recursive", "--force"},
		{"git", "-C", target, "submodule", "foreach", "--recursive", "git reset --hard && git clean -ffdx"},
	}
	for _, command := range commands {
		if err := execManaged(ctx, incus, instance, stdout, stderr, command); err != nil {
			return err
		}
	}
	return nil
}

func managedCommand(command []string) []string {
	result := make([]string, 0, len(managedCommandPrefix)+len(command))
	result = append(result, managedCommandPrefix...)
	return append(result, command...)
}

func execManaged(ctx context.Context, incus client, instance string, stdout, stderr io.Writer, command []string) error {
	_, err := execManagedOutput(ctx, incus, instance, stdout, stderr, command)
	return err
}

func execManagedOutput(ctx context.Context, incus client, instance string, stdout, stderr io.Writer, command []string) (string, error) {
	return execOutput(ctx, incus, instance, stdout, stderr, managedCommand(command))
}

func exec(ctx context.Context, incus client, instance string, stdout, stderr io.Writer, command []string) error {
	_, err := execOutput(ctx, incus, instance, stdout, stderr, command)
	return err
}

func execOutput(ctx context.Context, incus client, instance string, stdout, stderr io.Writer, command []string) (string, error) {
	commandStdout, commandStderr, err := incus.Exec(ctx, instance, incusclient.ExecRequest{Command: command})
	if commandStdout != "" {
		_, _ = fmt.Fprint(stdout, commandStdout)
	}
	if commandStderr != "" {
		_, _ = fmt.Fprint(stderr, commandStderr)
	}
	if err != nil {
		return commandStdout, fmt.Errorf("execute %q: %w", strings.Join(command, " "), err)
	}
	return commandStdout, nil
}
