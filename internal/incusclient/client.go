package incusclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	incus "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

const ProjectName = "kanedias"

var requiredProjectFeatures = map[string]string{
	"features.images":          "true",
	"features.profiles":        "true",
	"features.networks":        "false",
	"features.storage.volumes": "true",
}

type projectManager interface {
	GetProject(string) (*api.Project, string, error)
	CreateProject(api.ProjectsPost) error
}

type contextServer interface {
	incus.InstanceServer
	WithContext(context.Context) incus.InstanceServer
}

func ensureProject(server projectManager) error {
	project, _, err := server.GetProject(ProjectName)
	if err != nil {
		if !IsNotFound(err) {
			return fmt.Errorf("get Incus project %q: %w", ProjectName, err)
		}

		config := make(map[string]string, len(requiredProjectFeatures))
		for key, value := range requiredProjectFeatures {
			config[key] = value
		}
		if err := server.CreateProject(api.ProjectsPost{
			Name: ProjectName,
			ProjectPut: api.ProjectPut{
				Description: "Kanedias managed resources",
				Config:      config,
			},
		}); err != nil {
			return fmt.Errorf("create Incus project %q: %w", ProjectName, err)
		}
		return nil
	}

	if project == nil {
		return fmt.Errorf("get Incus project %q: returned no project", ProjectName)
	}
	for key, required := range requiredProjectFeatures {
		if project.Config[key] != required {
			return fmt.Errorf("incompatible feature %q on Incus project %q", key, ProjectName)
		}
	}
	return nil
}

func scopeProject(server contextServer) (contextServer, error) {
	scoped, ok := server.UseProject(ProjectName).(contextServer)
	if !ok {
		return nil, fmt.Errorf("select Incus project %q: client does not support request contexts", ProjectName)
	}
	return scoped, nil
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
	if err := ensureProject(contextual.WithContext(ctx)); err != nil {
		server.Disconnect()
		return nil, fmt.Errorf("connect to Incus: %w", err)
	}
	scoped, err := scopeProject(contextual)
	if err != nil {
		server.Disconnect()
		return nil, fmt.Errorf("connect to Incus: %w", err)
	}
	return &Client{server: scoped}, nil
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

type submittedOperationError struct {
	err    error
	local  operationWaiter
	remote *submittedRemoteOperation
}

func (e *submittedOperationError) Error() string { return e.err.Error() }
func (e *submittedOperationError) Unwrap() error { return e.err }

// OperationWasSubmitted reports whether an Incus request was accepted before its wait failed.
func OperationWasSubmitted(err error) bool {
	var submitted *submittedOperationError
	return errors.As(err, &submitted)
}

type operationWaiter interface {
	WaitContext(context.Context) error
	Get() api.Operation
}

func waitOperation(ctx context.Context, operation operationWaiter) error {
	return operation.WaitContext(ctx)
}

func submitAndWaitOperation(ctx context.Context, submit func() (operationWaiter, error)) error {
	operation, err := submit()
	if err != nil {
		return err
	}
	if err := waitOperation(ctx, operation); err != nil {
		return &submittedOperationError{err: err, local: operation}
	}
	return nil
}

type remoteOperationWaiter interface {
	Wait() error
	CancelTarget() error
}

type submittedRemoteOperation struct {
	waitEntered chan struct{}
	done        chan struct{}
	err         error
}

type observableRemoteOperationWaiter struct {
	remoteOperationWaiter
	waitEntered chan struct{}
}

func (o *observableRemoteOperationWaiter) Wait() error {
	close(o.waitEntered)
	return o.remoteOperationWaiter.Wait()
}

// RemoteOperationWaitObservation exposes read-only lifecycle signals for a
// submitted remote-operation waiter. WaitEntered closes at entry to the
// observable Wait method; Done closes only after that method returns.
type RemoteOperationWaitObservation struct {
	waitEntered <-chan struct{}
	done        <-chan struct{}
}

func (o RemoteOperationWaitObservation) WaitEntered() <-chan struct{} { return o.waitEntered }
func (o RemoteOperationWaitObservation) Done() <-chan struct{}        { return o.done }

type remoteOperationWaitHookKey struct{}

// WithRemoteOperationWaitHook installs a synchronization hook that runs after
// a remote operation has been submitted and its waiter has entered Wait. The
// observation lets destructive live validation reject an already-terminal
// wait immediately before cancellation.
func WithRemoteOperationWaitHook(ctx context.Context, hook func(RemoteOperationWaitObservation)) context.Context {
	return context.WithValue(ctx, remoteOperationWaitHookKey{}, hook)
}

func runRemoteOperationWaitHook(ctx context.Context, remote *submittedRemoteOperation) {
	hook, ok := ctx.Value(remoteOperationWaitHookKey{}).(func(RemoteOperationWaitObservation))
	if !ok || hook == nil {
		return
	}
	<-remote.waitEntered
	hook(RemoteOperationWaitObservation{waitEntered: remote.waitEntered, done: remote.done})
}

func startRemoteOperation(operation remoteOperationWaiter) *submittedRemoteOperation {
	remote := &submittedRemoteOperation{
		waitEntered: make(chan struct{}),
		done:        make(chan struct{}),
	}
	observable := &observableRemoteOperationWaiter{
		remoteOperationWaiter: operation,
		waitEntered:           remote.waitEntered,
	}
	go func() {
		remote.err = observable.Wait()
		close(remote.done)
	}()
	return remote
}

func submitAndWaitRemoteOperation(ctx context.Context, submit func() (remoteOperationWaiter, error)) error {
	operation, err := submit()
	if err != nil {
		return err
	}
	remote := startRemoteOperation(operation)
	runRemoteOperationWaitHook(ctx, remote)
	if err := waitSubmittedRemoteOperation(ctx, operation, remote); err != nil {
		return &submittedOperationError{err: err, remote: remote}
	}
	return nil
}

func submitAndWaitRemoteOperationUntilTerminal(ctx context.Context, submit func() (remoteOperationWaiter, error)) error {
	operation, err := submit()
	if err != nil {
		return err
	}
	remote := startRemoteOperation(operation)
	runRemoteOperationWaitHook(ctx, remote)
	if err := waitRemoteOperationUntilTerminal(ctx, operation, remote); err != nil {
		return &submittedOperationError{err: err, remote: remote}
	}
	return nil
}

func waitRemoteOperation(ctx context.Context, operation remoteOperationWaiter) error {
	return waitRemoteOperationUntilTerminal(ctx, operation, startRemoteOperation(operation))
}

func waitRemoteOperationUntilTerminal(ctx context.Context, operation remoteOperationWaiter, remote *submittedRemoteOperation) error {
	select {
	case <-remote.done:
		return remote.err
	case <-ctx.Done():
		cancelErr := operation.CancelTarget()
		<-remote.done
		return errors.Join(ctx.Err(), cancelErr, remote.err)
	}
}

func waitSubmittedRemoteOperation(ctx context.Context, operation remoteOperationWaiter, remote *submittedRemoteOperation) error {
	if ctx.Err() != nil {
		if cancelErr := operation.CancelTarget(); cancelErr != nil {
			return errors.Join(ctx.Err(), fmt.Errorf("cancel submitted Incus target: %w", cancelErr))
		}
		return ctx.Err()
	}
	select {
	case <-remote.done:
		return remote.err
	case <-ctx.Done():
		if cancelErr := operation.CancelTarget(); cancelErr != nil {
			return errors.Join(ctx.Err(), fmt.Errorf("cancel submitted Incus target: %w", cancelErr))
		}
		return ctx.Err()
	}
}

// AwaitSubmittedOperation waits for a previously submitted local create/start
// or remote copy to reach terminal state on a detached bounded cleanup context.
func AwaitSubmittedOperation(ctx context.Context, err error) error {
	var submitted *submittedOperationError
	if !errors.As(err, &submitted) {
		return fmt.Errorf("error does not retain a submitted operation")
	}
	if submitted.remote != nil {
		select {
		case <-submitted.remote.done:
			return submitted.remote.err
		case <-ctx.Done():
			return fmt.Errorf("await submitted Incus remote operation: %w", ctx.Err())
		}
	}
	if submitted.local != nil {
		if waitErr := submitted.local.WaitContext(ctx); waitErr != nil {
			return fmt.Errorf("await submitted Incus local operation: %w", waitErr)
		}
		return nil
	}
	return fmt.Errorf("error does not retain a submitted operation waiter")
}

// AwaitSubmittedRemoteOperation preserves the Task 4 remote-copy seam.
func AwaitSubmittedRemoteOperation(ctx context.Context, err error) error {
	var submitted *submittedOperationError
	if !errors.As(err, &submitted) || submitted.remote == nil {
		return fmt.Errorf("error does not retain a submitted remote operation")
	}
	return AwaitSubmittedOperation(ctx, err)
}
