package incusclient

import (
	"context"
	"fmt"
	"net/http"

	incus "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

type contextServer interface {
	incus.InstanceServer
	WithContext(context.Context) incus.InstanceServer
}

// Client is a thin, context-aware adapter over the Incus client.
type Client struct {
	server contextServer
}

func Connect(ctx context.Context) (*Client, error) {
	server, err := incus.ConnectIncusUnixWithContext(ctx, "", nil)
	if err != nil {
		return nil, fmt.Errorf("connect to Incus: %w", err)
	}

	contextual, ok := server.(contextServer)
	if !ok {
		server.Disconnect()
		return nil, fmt.Errorf("connect to Incus: client does not support request contexts")
	}
	return &Client{server: contextual}, nil
}

func (c *Client) Disconnect() {
	c.server.Disconnect()
}

type poolNameServer interface {
	GetStoragePoolNames() ([]string, error)
}

func (c *Client) ResolvePool(ctx context.Context, configured string) (string, error) {
	return resolvePool(c.server.WithContext(ctx), configured)
}

func resolvePool(server poolNameServer, configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}

	names, err := server.GetStoragePoolNames()
	if err != nil {
		return "", fmt.Errorf("list Incus storage pools: %w", err)
	}
	if len(names) != 1 {
		return "", fmt.Errorf("storage pool is not configured and Incus returned %d pools; expected exactly one", len(names))
	}
	return names[0], nil
}

func (c *Client) GetNetwork(ctx context.Context, name string) (*api.Network, error) {
	server := c.server.WithContext(ctx)
	network, _, err := server.GetNetwork(name)
	if err != nil {
		return nil, fmt.Errorf("get Incus network %q: %w", name, err)
	}
	return network, nil
}

func (c *Client) CreateNetwork(ctx context.Context, network api.NetworksPost) error {
	server := c.server.WithContext(ctx)
	if err := server.CreateNetwork(network); err != nil {
		return fmt.Errorf("create Incus network %q: %w", network.Name, err)
	}
	return nil
}

func IsNotFound(err error) bool {
	return api.StatusErrorCheck(err, http.StatusNotFound)
}

type operationWaiter interface {
	WaitContext(context.Context) error
}

func waitOperation(ctx context.Context, operation operationWaiter) error {
	return operation.WaitContext(ctx)
}

type remoteOperationWaiter interface {
	Wait() error
	CancelTarget() error
}

func waitRemoteOperation(ctx context.Context, operation remoteOperationWaiter) error {
	result := make(chan error, 1)
	go func() {
		result <- operation.Wait()
	}()

	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = operation.CancelTarget()
		return ctx.Err()
	}
}
