package incusclient

import (
	"context"
	"net/http"
	"strings"
	"testing"

	incus "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

var projectFeatures = map[string]string{
	"features.images":          "true",
	"features.profiles":        "true",
	"features.networks":        "true",
	"features.storage.volumes": "true",
}

type fakeProjectManager struct {
	project *api.Project
	getErr  error
	created *api.ProjectsPost
}

func (f *fakeProjectManager) GetProject(string) (*api.Project, string, error) {
	return f.project, "", f.getErr
}

func (f *fakeProjectManager) CreateProject(project api.ProjectsPost) error {
	f.created = &project
	return nil
}

func TestEnsureProjectCreatesMissingKanediasProject(t *testing.T) {
	fake := &fakeProjectManager{getErr: api.StatusErrorf(http.StatusNotFound, "missing")}

	if err := ensureProject(fake); err != nil {
		t.Fatal(err)
	}
	if fake.created == nil {
		t.Fatal("CreateProject was not called")
	}
	if fake.created.Name != ProjectName {
		t.Fatalf("created project = %q, want %q", fake.created.Name, ProjectName)
	}
	for _, key := range []string{
		"features.images",
		"features.profiles",
		"features.networks",
		"features.storage.volumes",
	} {
		if fake.created.Config[key] != "true" {
			t.Errorf("created feature %q = %q, want true", key, fake.created.Config[key])
		}
	}
}

func TestEnsureProjectAcceptsRequiredFeatures(t *testing.T) {
	fake := &fakeProjectManager{project: &api.Project{ProjectPut: api.ProjectPut{Config: projectFeatures}}}

	if err := ensureProject(fake); err != nil {
		t.Fatal(err)
	}
	if fake.created != nil {
		t.Fatal("CreateProject was called for an existing compatible project")
	}
}

func TestEnsureProjectRejectsIncompatibleFeatures(t *testing.T) {
	for key := range projectFeatures {
		t.Run(key, func(t *testing.T) {
			features := make(map[string]string, len(projectFeatures))
			for feature, value := range projectFeatures {
				features[feature] = value
			}
			features[key] = "false"
			fake := &fakeProjectManager{project: &api.Project{ProjectPut: api.ProjectPut{Config: features}}}

			err := ensureProject(fake)
			if err == nil {
				t.Fatal("ensureProject() error = nil, want incompatible feature error")
			}
			if !strings.Contains(err.Error(), ProjectName) || !strings.Contains(err.Error(), key) {
				t.Fatalf("ensureProject() error = %q, want project %q and feature %q", err, ProjectName, key)
			}
		})
	}
}

type fakeContextServer struct {
	incus.InstanceServer
	selected string
	scoped   incus.InstanceServer
}

func (f *fakeContextServer) UseProject(name string) incus.InstanceServer {
	f.selected = name
	return f.scoped
}

func (f *fakeContextServer) WithContext(context.Context) incus.InstanceServer {
	return f
}

func TestScopeProjectSelection(t *testing.T) {
	scoped := &fakeContextServer{}
	server := &fakeContextServer{scoped: scoped}

	got, err := scopeProject(server)
	if err != nil {
		t.Fatal(err)
	}
	if server.selected != ProjectName {
		t.Fatalf("selected project = %q, want %q", server.selected, ProjectName)
	}
	if got != scoped {
		t.Fatal("scopeProject() did not return scoped server")
	}
}

type poolServer struct {
	names []string
	err   error
	calls int
}

func (s *poolServer) GetStoragePoolNames() ([]string, error) {
	s.calls++
	return s.names, s.err
}

func TestResolvePoolUsesConfiguredNameWithoutQuery(t *testing.T) {
	server := &poolServer{}
	got, err := resolvePool(server, "fast")
	if err != nil {
		t.Fatal(err)
	}
	if got != "fast" {
		t.Fatalf("resolvePool() = %q, want fast", got)
	}
	if server.calls != 0 {
		t.Fatalf("GetStoragePoolNames calls = %d, want 0", server.calls)
	}
}

func TestResolvePoolRequiresExactlyOnePoolWhenUnconfigured(t *testing.T) {
	tests := []struct {
		name    string
		names   []string
		want    string
		wantErr bool
	}{
		{name: "one", names: []string{"default"}, want: "default"},
		{name: "zero", wantErr: true},
		{name: "multiple", names: []string{"default", "fast"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePool(&poolServer{names: tt.names}, "")
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolvePool() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("resolvePool() = %q, want %q", got, tt.want)
			}
		})
	}
}

type fakeOperation struct {
	waitContext context.Context
	waitErr     error
	operation   api.Operation
}

func (o *fakeOperation) WaitContext(ctx context.Context) error {
	o.waitContext = ctx
	return o.waitErr
}

func (o *fakeOperation) Get() api.Operation {
	return o.operation
}

func TestWaitOperationPassesContext(t *testing.T) {
	key := struct{}{}
	ctx := context.WithValue(context.Background(), key, "value")
	op := &fakeOperation{}
	if err := waitOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if op.waitContext != ctx {
		t.Fatal("WaitContext did not receive supplied context")
	}
}
