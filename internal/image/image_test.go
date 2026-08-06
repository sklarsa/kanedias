package image

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
)

func TestCreateRunsImageWorkflowInOrder(t *testing.T) {
	cfg := imageConfig(t, []string{"github.com", "gitlab.com"})
	client := &recordingClient{files: make(map[string]uploadedFile)}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := create(context.Background(), cfg, &stdout, &stderr, func(context.Context) (imageClient, error) {
		return client, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	wantCalls := []string{
		"resolve-pool",
		"get-network kanedias",
		"create-network kanedias",
		"ensure-profile image-build",
		"create-instance",
		"push /root/install.sh",
		"exec install -d /root/assets",
		"push /root/assets/authorized_hosts",
		"push /root/assets/pi-settings.json",
		"push /root/assets/pi-auth.json",
		"push /root/assets/cobalt-ember.json",
		"push /root/assets/tmux.conf",
		"push /root/assets/kanedias-pi.socket",
		"push /root/assets/kanedias-pi@.service",
		"push /root/assets/kanedias-pi-rpc",
		"exec bash /root/install.sh",
		"stop",
		"publish",
		"cleanup-delete-instance",
	}
	if !reflect.DeepEqual(client.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", client.calls, wantCalls)
	}
	if client.profileDefinition == nil {
		t.Fatal("image-build profile definition was not supplied")
	}
	for _, want := range []string{"eth0:", "network: kanedias", "type: nic"} {
		if !strings.Contains(string(client.profileDefinition), want) {
			t.Errorf("image-build profile missing %q:\n%s", want, client.profileDefinition)
		}
	}

	request := client.createRequest
	if !strings.HasPrefix(request.Name, "image-build-") {
		t.Errorf("instance name = %q, want image-build prefix", request.Name)
	}
	if request.Type != api.InstanceTypeContainer || !request.Start {
		t.Errorf("instance type/start = %q/%v, want container/true", request.Type, request.Start)
	}
	if got, want := request.Profiles, []string{"default", "image-build"}; !reflect.DeepEqual(got, want) {
		t.Errorf("profiles = %#v, want %#v", got, want)
	}
	wantRoot := map[string]string{"type": "disk", "pool": "pool1", "path": "/"}
	if got := request.Devices["root"]; !reflect.DeepEqual(got, wantRoot) {
		t.Errorf("root device = %#v, want %#v", got, wantRoot)
	}
	if request.Source.Type != "image" || request.Source.Server != cfg.BaseImage.Source || request.Source.Protocol != "simplestreams" || request.Source.Alias != cfg.BaseImage.Image {
		t.Errorf("instance source = %#v", request.Source)
	}
	if got := string(client.files["/root/assets/authorized_hosts"].content); got != "github.com\ngitlab.com" {
		t.Errorf("authorized_hosts = %q, want newline-joined hosts", got)
	}
	socket := string(client.files["/root/assets/kanedias-pi.socket"].content)
	if !strings.Contains(socket, "ListenStream=0.0.0.0:7777") ||
		!strings.Contains(socket, "Accept=yes") ||
		!strings.Contains(socket, "MaxConnections=1") {
		t.Fatalf("socket unit = %q", socket)
	}
	service := string(client.files["/root/assets/kanedias-pi@.service"].content)
	for _, want := range []string{
		"User=kanedias",
		"WorkingDirectory=/workspace",
		"StandardInput=socket",
		"StandardOutput=inherit",
		"StandardError=journal",
	} {
		if !strings.Contains(service, want) {
			t.Errorf("service unit missing %q", want)
		}
	}
	auth := client.files["/root/assets/pi-auth.json"]
	if got := string(auth.content); got != "auth" {
		t.Errorf("pi auth = %q, want test asset", got)
	}
	if auth.mode != 0o600 {
		t.Errorf("pi auth mode = %#o, want 0600", auth.mode)
	}
	launcher := client.files["/root/assets/kanedias-pi-rpc"]
	if !strings.Contains(string(launcher.content), "exec pi --mode rpc --no-session") {
		t.Fatalf("launcher = %q", launcher.content)
	}
	if launcher.mode != 0o700 {
		t.Errorf("launcher mode = %#o, want 0700", launcher.mode)
	}
	for _, path := range []string{
		"/root/assets/kanedias-pi.socket",
		"/root/assets/kanedias-pi@.service",
	} {
		if client.files[path].mode != 0o644 {
			t.Errorf("%s mode = %#o, want 0644", path, client.files[path].mode)
		}
	}
	if got, want := client.publishAlias, cfg.BaseImage.Name; got != want {
		t.Errorf("published alias = %q, want %q", got, want)
	}
	if got, want := client.publishDescription, "kanedias sandbox from https://images.linuxcontainers.org/debian/13"; got != want {
		t.Errorf("published description = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestCreateUploadsEmptyAuthorizedHosts(t *testing.T) {
	cfg := imageConfig(t, nil)
	client := &recordingClient{files: make(map[string]uploadedFile)}

	if err := create(context.Background(), cfg, io.Discard, io.Discard, func(context.Context) (imageClient, error) {
		return client, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := client.files["/root/assets/authorized_hosts"].content; len(got) != 0 {
		t.Errorf("authorized_hosts = %q, want empty file", got)
	}
}

func TestCreateUsesRequestDerivedNonCanceledBoundedContextForCleanup(t *testing.T) {
	cfg := imageConfig(t, nil)
	const sentinel = "request-value"
	ctx := context.WithValue(context.Background(), imageRequestContextKey{}, sentinel)
	ctx, cancel := context.WithCancel(ctx)
	client := &recordingClient{
		files: make(map[string]uploadedFile),
		exec: func() error {
			cancel()
			return context.Canceled
		},
	}

	err := create(ctx, cfg, io.Discard, io.Discard, func(context.Context) (imageClient, error) {
		return client, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("create() error = %v, want context cancellation", err)
	}
	if got, want := client.calls[len(client.calls)-2:], []string{"cleanup-stop", "cleanup-delete-instance"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup calls = %#v, want %#v", got, want)
	}
	if len(client.cleanupContexts) != 2 {
		t.Fatalf("cleanup contexts = %d, want 2", len(client.cleanupContexts))
	}
	for _, observed := range client.cleanupContexts {
		if observed.err != nil {
			t.Errorf("cleanup context error = %v, want non-canceled context", observed.err)
		}
		if observed.deadlineRemaining <= 0 || observed.deadlineRemaining > 30*time.Second {
			t.Errorf("cleanup deadline remaining = %v, want bounded by 30s", observed.deadlineRemaining)
		}
		if observed.value != sentinel {
			t.Errorf("cleanup context value = %v, want %q", observed.value, sentinel)
		}
	}
}

func TestCreateJoinsPrimaryAndRunningInstanceCleanupErrors(t *testing.T) {
	cfg := imageConfig(t, nil)
	primaryErr := errors.New("installer failed")
	stopErr := errors.New("cleanup stop failed")
	client := &recordingClient{
		files:   make(map[string]uploadedFile),
		exec:    func() error { return primaryErr },
		stopErr: stopErr,
	}

	err := create(context.Background(), cfg, io.Discard, io.Discard, func(context.Context) (imageClient, error) {
		return client, nil
	})
	for _, want := range []error{primaryErr, stopErr, errDeleteRunningInstance} {
		if !errors.Is(err, want) {
			t.Errorf("create() error = %v, want errors.Is(_, %v)", err, want)
		}
	}
	if got, want := client.calls[len(client.calls)-2:], []string{"cleanup-stop", "cleanup-delete-instance"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup calls = %#v, want %#v", got, want)
	}
}

func TestCreateReadsAssetsBeforeConnecting(t *testing.T) {
	cfg := imageConfig(t, nil)
	if err := os.Remove(cfg.AssetPath("tmux.conf")); err != nil {
		t.Fatal(err)
	}
	connected := false

	err := create(context.Background(), cfg, io.Discard, io.Discard, func(context.Context) (imageClient, error) {
		connected = true
		return &recordingClient{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "tmux.conf") {
		t.Fatalf("create() error = %v, want missing asset error", err)
	}
	if connected {
		t.Fatal("connected to Incus before all assets were read")
	}
}

func TestCreateValidatesBeforeConnecting(t *testing.T) {
	cfg := imageConfig(t, nil)
	cfg.BaseImage.Name = ""
	connected := false

	err := create(context.Background(), cfg, io.Discard, io.Discard, func(context.Context) (imageClient, error) {
		connected = true
		return &recordingClient{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "base_image.name is required") {
		t.Fatalf("create() error = %v, want validation error", err)
	}
	if connected {
		t.Fatal("connected to Incus before validating lifecycle config")
	}
}

func imageConfig(t *testing.T, hosts []string) config.Config {
	t.Helper()
	dir := t.TempDir()
	assetDir := filepath.Join(dir, "assets")
	if err := os.Mkdir(assetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"pi-settings.json":  "settings",
		"pi-auth.json":      "auth",
		"cobalt-ember.json": "theme",
		"tmux.conf":         "tmux",
	} {
		if err := os.WriteFile(filepath.Join(assetDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return config.Config{
		Dir:     dir,
		Network: config.Network{IPv4: "10.76.111.1/24"},
		BaseImage: config.BaseImage{
			Name:            "sandbox",
			Source:          "https://images.linuxcontainers.org",
			Image:           "debian/13",
			AuthorizedHosts: hosts,
		},
	}
}

type imageRequestContextKey struct{}

type cleanupContextObservation struct {
	err               error
	deadlineRemaining time.Duration
	value             any
}

var errDeleteRunningInstance = errors.New("cannot delete running instance")

type uploadedFile struct {
	content []byte
	mode    int
}

type recordingClient struct {
	calls              []string
	files              map[string]uploadedFile
	profileDefinition  []byte
	createRequest      api.InstancesPost
	publishAlias       string
	publishDescription string
	exec               func() error
	running            bool
	stopErr            error
	cleanupContexts    []cleanupContextObservation
}

func (c *recordingClient) ResolvePool(context.Context, string) (string, error) {
	c.calls = append(c.calls, "resolve-pool")
	return "pool1", nil
}

func (c *recordingClient) GetNetwork(context.Context, string) (*api.Network, error) {
	c.calls = append(c.calls, "get-network kanedias")
	return nil, api.StatusErrorf(404, "missing")
}

func (c *recordingClient) CreateNetwork(_ context.Context, request api.NetworksPost) error {
	c.calls = append(c.calls, "create-network "+request.Name)
	return nil
}

func (c *recordingClient) EnsureProfile(_ context.Context, name string, definition []byte) error {
	c.calls = append(c.calls, "ensure-profile "+name)
	c.profileDefinition = append([]byte(nil), definition...)
	return nil
}

func (c *recordingClient) CreateInstance(_ context.Context, request api.InstancesPost) error {
	c.calls = append(c.calls, "create-instance")
	c.createRequest = request
	c.running = request.Start
	return nil
}

func (c *recordingClient) PushFile(_ context.Context, _ string, path string, content []byte, mode int) error {
	c.calls = append(c.calls, "push "+path)
	c.files[path] = uploadedFile{content: append([]byte(nil), content...), mode: mode}
	return nil
}

func (c *recordingClient) Exec(_ context.Context, _ string, request incusclient.ExecRequest) (string, string, error) {
	c.calls = append(c.calls, "exec "+strings.Join(request.Command, " "))
	if c.exec != nil {
		return "", "", c.exec()
	}
	return "installer output", "", nil
}

func (c *recordingClient) StopInstance(ctx context.Context, _ string, force bool) error {
	if _, cleanup := ctx.Deadline(); cleanup {
		c.calls = append(c.calls, "cleanup-stop")
		c.observeCleanupContext(ctx)
		if !force {
			return errors.New("cleanup stop was not forced")
		}
	} else {
		c.calls = append(c.calls, "stop")
	}
	if c.stopErr != nil {
		return c.stopErr
	}
	c.running = false
	return nil
}

func (c *recordingClient) PublishInstance(_ context.Context, _ string, alias, description string) error {
	c.calls = append(c.calls, "publish")
	c.publishAlias = alias
	c.publishDescription = description
	return nil
}

func (c *recordingClient) DeleteInstance(ctx context.Context, _ string) error {
	c.calls = append(c.calls, "cleanup-delete-instance")
	c.observeCleanupContext(ctx)
	if c.running {
		return errDeleteRunningInstance
	}
	return nil
}

func (c *recordingClient) observeCleanupContext(ctx context.Context) {
	deadline, _ := ctx.Deadline()
	c.cleanupContexts = append(c.cleanupContexts, cleanupContextObservation{
		err:               ctx.Err(),
		deadlineRemaining: time.Until(deadline),
		value:             ctx.Value(imageRequestContextKey{}),
	})
}

func (c *recordingClient) Disconnect() {}
