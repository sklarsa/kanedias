package incusclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"sync"

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

type captureBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *captureBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *captureBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func exec(ctx context.Context, call execCall, request ExecRequest) (stdout, stderr string, err error) {
	var stdoutBuffer captureBuffer
	var stderrBuffer captureBuffer
	dataDone := make(chan bool)
	operation, err := call(api.InstanceExecPost{
		Command:     request.Command,
		Environment: request.Environment,
		Cwd:         request.Cwd,
		WaitForWS:   true,
		Interactive: false,
	}, &incus.InstanceExecArgs{
		Stdin:    request.Stdin,
		Stdout:   &stdoutBuffer,
		Stderr:   &stderrBuffer,
		DataDone: dataDone,
	})
	if err != nil {
		return stdoutBuffer.String(), stderrBuffer.String(), fmt.Errorf("execute command in Incus instance: %w", err)
	}

	waitErr := waitOperation(ctx, operation)
	if waitErr != nil {
		cancelExecOperation(operation)
		return stdoutBuffer.String(), stderrBuffer.String(), fmt.Errorf("wait for command in Incus instance: %w", waitErr)
	}
	select {
	case <-dataDone:
	case <-ctx.Done():
		cancelExecOperation(operation)
		return stdoutBuffer.String(), stderrBuffer.String(), fmt.Errorf("flush command output from Incus instance: %w", ctx.Err())
	}

	status, err := execReturnStatus(operation.Get().Metadata)
	if err != nil {
		return stdoutBuffer.String(), stderrBuffer.String(), err
	}
	if status != 0 {
		return stdoutBuffer.String(), stderrBuffer.String(), fmt.Errorf("command in Incus instance exited with exit status %d", status)
	}
	return stdoutBuffer.String(), stderrBuffer.String(), nil
}

func cancelExecOperation(operation operationWaiter) {
	if canceler, ok := operation.(interface{ Cancel() error }); ok {
		_ = canceler.Cancel()
	}
}

func execReturnStatus(metadata map[string]any) (int, error) {
	raw, ok := metadata["return"]
	if !ok {
		return 0, fmt.Errorf("command in Incus instance has missing return metadata")
	}
	value, ok := raw.(float64)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
		return 0, fmt.Errorf("command in Incus instance has malformed return metadata %v", raw)
	}
	status := int(value)
	if float64(status) != value {
		return 0, fmt.Errorf("command in Incus instance has out-of-range return metadata %v", raw)
	}
	return status, nil
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
