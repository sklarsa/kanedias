package incusclient

import (
	"bytes"
	"context"
	"fmt"
	"io"

	incus "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

type ExecRequest struct {
	Command     []string
	Environment map[string]string
	Cwd         string
	Stdin       io.Reader
}

func (c *Client) GetInstance(ctx context.Context, name string) (*api.Instance, string, error) {
	server := c.server.WithContext(ctx)
	instance, etag, err := server.GetInstance(name)
	if err != nil {
		return nil, "", fmt.Errorf("get Incus instance %q: %w", name, err)
	}
	return instance, etag, nil
}

func (c *Client) CreateInstance(ctx context.Context, request api.InstancesPost) error {
	server := c.server.WithContext(ctx)
	operation, err := server.CreateInstance(request)
	if err != nil {
		return fmt.Errorf("create Incus instance %q: %w", request.Name, err)
	}
	if err := waitOperation(ctx, operation); err != nil {
		return fmt.Errorf("create Incus instance %q: %w", request.Name, err)
	}
	return nil
}

func (c *Client) UpdateInstance(ctx context.Context, name string, request api.InstancePut, etag string) error {
	server := c.server.WithContext(ctx)
	operation, err := server.UpdateInstance(name, request, etag)
	if err != nil {
		return fmt.Errorf("update Incus instance %q: %w", name, err)
	}
	if err := waitOperation(ctx, operation); err != nil {
		return fmt.Errorf("update Incus instance %q: %w", name, err)
	}
	return nil
}

func (c *Client) StartInstance(ctx context.Context, name string) error {
	return c.changeInstanceState(ctx, name, api.InstanceStatePut{Action: "start"})
}

func (c *Client) StopInstance(ctx context.Context, name string, force bool) error {
	return c.changeInstanceState(ctx, name, api.InstanceStatePut{Action: "stop", Force: force})
}

func (c *Client) changeInstanceState(ctx context.Context, name string, request api.InstanceStatePut) error {
	server := c.server.WithContext(ctx)
	operation, err := server.UpdateInstanceState(name, request, "")
	if err != nil {
		return fmt.Errorf("%s Incus instance %q: %w", request.Action, name, err)
	}
	if err := waitOperation(ctx, operation); err != nil {
		return fmt.Errorf("%s Incus instance %q: %w", request.Action, name, err)
	}
	return nil
}

func (c *Client) DeleteInstance(ctx context.Context, name string) error {
	server := c.server.WithContext(ctx)
	operation, err := server.DeleteInstance(name)
	if err != nil {
		return fmt.Errorf("delete Incus instance %q: %w", name, err)
	}
	if err := waitOperation(ctx, operation); err != nil {
		return fmt.Errorf("delete Incus instance %q: %w", name, err)
	}
	return nil
}

func (c *Client) Exec(ctx context.Context, name string, request ExecRequest) (stdout, stderr string, err error) {
	server := c.server.WithContext(ctx)
	return exec(ctx, func(post api.InstanceExecPost, args *incus.InstanceExecArgs) (operationWaiter, error) {
		return server.ExecInstance(name, post, args)
	}, request)
}

type execCall func(api.InstanceExecPost, *incus.InstanceExecArgs) (operationWaiter, error)

func exec(ctx context.Context, call execCall, request ExecRequest) (stdout, stderr string, err error) {
	var stdoutBuffer bytes.Buffer
	var stderrBuffer bytes.Buffer
	operation, err := call(api.InstanceExecPost{
		Command:     request.Command,
		Environment: request.Environment,
		Cwd:         request.Cwd,
		WaitForWS:   true,
		Interactive: false,
	}, &incus.InstanceExecArgs{
		Stdin:  request.Stdin,
		Stdout: &stdoutBuffer,
		Stderr: &stderrBuffer,
	})
	if err != nil {
		return "", "", fmt.Errorf("execute command in Incus instance: %w", err)
	}
	if err := waitOperation(ctx, operation); err != nil {
		return stdoutBuffer.String(), stderrBuffer.String(), fmt.Errorf("wait for command in Incus instance: %w", err)
	}
	return stdoutBuffer.String(), stderrBuffer.String(), nil
}

func (c *Client) PushFile(ctx context.Context, name, path string, content []byte, mode int) error {
	server := c.server.WithContext(ctx)
	if err := server.CreateInstanceFile(name, path, incus.InstanceFileArgs{
		Content:   bytes.NewReader(content),
		Mode:      mode,
		Type:      "file",
		WriteMode: "overwrite",
	}); err != nil {
		return fmt.Errorf("push file %q to Incus instance %q: %w", path, name, err)
	}
	return nil
}

func (c *Client) PublishInstance(ctx context.Context, name, alias, description string) error {
	server := c.server.WithContext(ctx)
	_, _, err := server.GetImageAlias(alias)
	switch {
	case err == nil:
		if err := server.DeleteImageAlias(alias); err != nil {
			return fmt.Errorf("delete existing Incus image alias %q: %w", alias, err)
		}
	case IsNotFound(err):
		// The alias is available.
	default:
		return fmt.Errorf("get Incus image alias %q: %w", alias, err)
	}

	operation, err := server.CreateImage(api.ImagesPost{
		ImagePut: api.ImagePut{
			Properties: map[string]string{"description": description},
		},
		Source: &api.ImagesPostSource{
			Type: "instance",
			Name: name,
		},
		Aliases: []api.ImageAlias{{Name: alias}},
	}, nil)
	if err != nil {
		return fmt.Errorf("publish Incus instance %q: %w", name, err)
	}
	if err := waitOperation(ctx, operation); err != nil {
		return fmt.Errorf("publish Incus instance %q: %w", name, err)
	}
	return nil
}
