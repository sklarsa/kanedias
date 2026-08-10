package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	WorkspaceRoot            = "/workspace"
	WorkspaceRepositoriesRoot = "/workspace/repos"
)

var repositoryComponent = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// WorkspaceRepository is a canonical, validated owner/repository start derived
// from a configured slug string.
type WorkspaceRepository struct {
	Slug     string
	Checkout string
}

// WorkspaceStart identifies a single repository to check out into the
// workspace during a session launch.
type WorkspaceStart struct {
	Repository string `json:"repository,omitempty"`
	Checkout   string `json:"checkout,omitempty"`
}

// ParseWorkspaceRepositories validates that each slug names exactly one
// GitHub owner/repository pair whose components are safe, and returns
// canonical, slug-sorted copies. Checkout basenames must be unique.
func ParseWorkspaceRepositories(slugs []string) ([]WorkspaceRepository, error) {
	repositories := make([]WorkspaceRepository, 0, len(slugs))
	checkouts := make(map[string]struct{}, len(slugs))
	for _, slug := range slugs {
		parts := strings.Split(slug, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid GitHub repository slug: %s", slug)
		}
		owner, name := parts[0], parts[1]
		if !repositoryComponent.MatchString(owner) || !repositoryComponent.MatchString(name) {
			return nil, fmt.Errorf("invalid GitHub repository slug: %s", slug)
		}
		if name == "." || name == ".." {
			return nil, fmt.Errorf("invalid GitHub repository slug: %s", slug)
		}
		if _, exists := checkouts[name]; exists {
			return nil, fmt.Errorf("duplicate repository destination: %s", name)
		}
		checkouts[name] = struct{}{}
		repositories = append(repositories, WorkspaceRepository{Slug: slug, Checkout: name})
	}
	sort.Slice(repositories, func(i, j int) bool { return repositories[i].Slug < repositories[j].Slug })
	return repositories, nil
}

// Validate returns an error when the start does not describe a valid
// repository checkout. An empty start is the workspace default.
func (start WorkspaceStart) Validate() error {
	if start.Repository == "" && start.Checkout == "" {
		return nil
	}
	repos, err := ParseWorkspaceRepositories([]string{start.Repository})
	if err != nil {
		return err
	}
	if len(repos) != 1 || repos[0].Checkout != start.Checkout {
		return fmt.Errorf("workspace start checkout does not match repository")
	}
	return nil
}

// Directory returns the canonical filesystem location for the start.
func (start WorkspaceStart) Directory() string {
	if start.Repository == "" {
		return WorkspaceRoot
	}
	return filepath.Join(WorkspaceRepositoriesRoot, start.Checkout)
}
