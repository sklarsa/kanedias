package incusclient

import (
	"context"
	"fmt"

	"github.com/lxc/incus/v7/shared/api"
	"gopkg.in/yaml.v3"
)

type profileAPI interface {
	GetProfile(string) (*api.Profile, string, error)
	CreateProfile(api.ProfilesPost) error
	UpdateProfile(string, api.ProfilePut, string) error
}

func (c *Client) EnsureProfile(ctx context.Context, name string, definition []byte) error {
	return ensureProfile(c.server.WithContext(ctx), name, definition)
}

func ensureProfile(server profileAPI, name string, definition []byte) error {
	var profile api.ProfilePut
	if err := yaml.Unmarshal(definition, &profile); err != nil {
		return fmt.Errorf("decode Incus profile %q: %w", name, err)
	}

	_, etag, err := server.GetProfile(name)
	if IsNotFound(err) {
		if err := server.CreateProfile(api.ProfilesPost{Name: name, ProfilePut: profile}); err != nil {
			return fmt.Errorf("create Incus profile %q: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Incus profile %q: %w", name, err)
	}
	if err := server.UpdateProfile(name, profile, etag); err != nil {
		return fmt.Errorf("update Incus profile %q: %w", name, err)
	}
	return nil
}
