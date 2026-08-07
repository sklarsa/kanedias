package workspace

import (
	"context"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/sklarsa/kanedias/internal/incusclient"
)

type repository struct {
	slug string
	name string
	url  string
}

func parseRepositories(slugs []string) ([]repository, error) {
	repositories := make([]repository, 0, len(slugs))
	destinations := make(map[string]struct{}, len(slugs))
	for _, slug := range slugs {
		parts := strings.Split(slug, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.IndexFunc(slug, unicode.IsSpace) >= 0 {
			return nil, fmt.Errorf("invalid GitHub repository slug: %s", slug)
		}
		name := parts[1]
		if name == "." || name == ".." {
			return nil, fmt.Errorf("invalid GitHub repository slug: %s", slug)
		}
		if _, exists := destinations[name]; exists {
			return nil, fmt.Errorf("duplicate repository destination: %s", name)
		}
		destinations[name] = struct{}{}
		repositories = append(repositories, repository{
			slug: slug,
			name: name,
			url:  "https://github.com/" + slug + ".git",
		})
	}
	return repositories, nil
}

var managedCommandPrefix = []string{
	"runuser", "-u", managedUser, "--",
	"env", "HOME=" + managedHome, "USER=" + managedUser, "LOGNAME=" + managedUser,
}

func prepareRepositoryRoot(ctx context.Context, incus client, instance string, stdout, stderr io.Writer) error {
	if err := exec(ctx, incus, instance, stdout, stderr, []string{"test", "!", "-L", workspacePath + "/repos"}); err != nil {
		return fmt.Errorf("refusing symlinked repository root: %s/repos: %w", workspacePath, err)
	}
	if err := exec(ctx, incus, instance, stdout, stderr, []string{"test", "!", "-e", workspacePath + "/repos", "-o", "-d", workspacePath + "/repos"}); err != nil {
		return fmt.Errorf("repository root is not a directory: %s/repos: %w", workspacePath, err)
	}
	return exec(ctx, incus, instance, stdout, stderr, []string{
		"install", "-d", "-o", managedUser, "-g", managedUser, workspacePath + "/repos",
	})
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
