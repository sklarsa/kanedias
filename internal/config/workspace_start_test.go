package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseWorkspaceRepositories(t *testing.T) {
	got, err := ParseWorkspaceRepositories([]string{"two/beta", "one/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	want := []WorkspaceRepository{
		{Slug: "one/alpha", Checkout: "alpha"},
		{Slug: "two/beta", Checkout: "beta"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestParseWorkspaceRepositoriesInvalid(t *testing.T) {
	tests := []struct {
		name string
		slug string
		want string
	}{
		{name: "missing owner", slug: "repo", want: "invalid GitHub repository slug"},
		{name: "missing repository", slug: "owner/", want: "invalid GitHub repository slug"},
		{name: "missing owner empty", slug: "/repo", want: "invalid GitHub repository slug"},
		{name: "extra separator", slug: "owner/repo/extra", want: "invalid GitHub repository slug"},
		{name: "whitespace in owner", slug: "my owner/repo", want: "invalid GitHub repository slug"},
		{name: "whitespace in repo", slug: "owner/my repo", want: "invalid GitHub repository slug"},
		{name: "tab in repo", slug: "owner/my\trepo", want: "invalid GitHub repository slug"},
		{name: "newline in repo", slug: "owner/my\nrepo", want: "invalid GitHub repository slug"},
		{name: "dot checkout", slug: "owner/.", want: "invalid GitHub repository slug"},
		{name: "dotdot checkout", slug: "owner/..", want: "invalid GitHub repository slug"},
		{name: "slash in repo", slug: "owner/repo/sub", want: "invalid GitHub repository slug"},
		{name: "backslash in repo", slug: "owner/repo\\sub", want: "invalid GitHub repository slug"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseWorkspaceRepositories([]string{tt.slug}); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ParseWorkspaceRepositories(%q) error = %v, want containing %q", tt.slug, err, tt.want)
			}
		})
	}
}

func TestParseWorkspaceRepositoriesDuplicateCheckoutBasename(t *testing.T) {
	if _, err := ParseWorkspaceRepositories([]string{"one/repo", "two/repo"}); err == nil || !strings.Contains(err.Error(), "duplicate repository destination") {
		t.Fatalf("duplicate basename error = %v, want duplicate repository destination", err)
	}
}

func TestWorkspaceStartValidationAndDirectory(t *testing.T) {
	tests := []struct {
		name    string
		start   WorkspaceStart
		wantDir string
		wantErr bool
	}{
		{name: "workspace default", wantDir: "/workspace"},
		{name: "configured checkout", start: WorkspaceStart{Repository: "owner/repo", Checkout: "repo"}, wantDir: "/workspace/repos/repo"},
		{name: "mismatched checkout", start: WorkspaceStart{Repository: "owner/repo", Checkout: "other"}, wantErr: true},
		{name: "browser path forbidden", start: WorkspaceStart{Repository: "owner/repo", Checkout: "../repo"}, wantErr: true},
		{name: "empty repository with checkout", start: WorkspaceStart{Checkout: "repo"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.start.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Validate() = nil, want error for start %#v", tt.start)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v for start %#v", err, tt.start)
			}
			if got := tt.start.Directory(); got != tt.wantDir {
				t.Fatalf("Directory() = %q, want %q", got, tt.wantDir)
			}
		})
	}
}
