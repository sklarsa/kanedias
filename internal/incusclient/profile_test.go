package incusclient

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
)

type profileServer struct {
	profile *api.Profile
	etag    string
	getErr  error
	created *api.ProfilesPost
	updated *api.ProfilePut
}

func (s *profileServer) GetProfile(string) (*api.Profile, string, error) {
	return s.profile, s.etag, s.getErr
}
func (s *profileServer) CreateProfile(profile api.ProfilesPost) error {
	s.created = &profile
	return nil
}
func (s *profileServer) UpdateProfile(_ string, profile api.ProfilePut, _ string) error {
	s.updated = &profile
	return nil
}

var profileYAML = []byte(`description: test profile
config:
  security.nesting: "true"
devices:
  root:
    type: disk
    path: /
    pool: default
`)

func TestEnsureProfileCreatesDecodedYAMLWhenMissing(t *testing.T) {
	server := &profileServer{getErr: api.StatusErrorf(http.StatusNotFound, "missing")}
	if err := ensureProfile(server, "sandbox", profileYAML); err != nil {
		t.Fatal(err)
	}
	if server.created == nil {
		t.Fatal("CreateProfile was not called")
	}
	if server.created.Name != "sandbox" || server.created.Description != "test profile" {
		t.Fatalf("created profile = %#v", server.created)
	}
	wantDevice := map[string]string{"type": "disk", "path": "/", "pool": "default"}
	if !reflect.DeepEqual(map[string]string(server.created.Devices["root"]), wantDevice) {
		t.Fatalf("root device = %#v, want %#v", server.created.Devices["root"], wantDevice)
	}
	if server.updated != nil {
		t.Fatal("UpdateProfile called on create path")
	}
}

func TestEnsureProfileUpdatesDecodedYAMLWhenPresent(t *testing.T) {
	server := &profileServer{profile: &api.Profile{}, etag: "etag"}
	if err := ensureProfile(server, "sandbox", profileYAML); err != nil {
		t.Fatal(err)
	}
	if server.updated == nil || server.updated.Description != "test profile" {
		t.Fatalf("updated profile = %#v", server.updated)
	}
	if server.created != nil {
		t.Fatal("CreateProfile called on update path")
	}
}
