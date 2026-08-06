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
	client := &recordingClient{files: make(map[string][]byte)}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := create(context.Background(), cfg, &stdout, &stderr, func(context.Context) (imageClient, error) {
		return client, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	wantCalls := []string{
		"ensure-profile image-build",
		"create-instance",
		"push /root/install.sh",
		"exec install -d /root/assets",
		"push /root/assets/authorized_hosts",
		"push /root/assets/pi-settings.json",
		"push /root/assets/cobalt-ember.json",
		"push /root/assets/tmux.conf",
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
	if request.Source.Type != "image" || request.Source.Server != cfg.BaseImage.Source || request.Source.Protocol != "simplestreams" || request.Source.Alias != cfg.BaseImage.Image {
		t.Errorf("instance source = %#v", request.Source)
	}
	if got := string(client.files["/root/assets/authorized_hosts"]); got != "github.com\ngitlab.com" {
		t.Errorf("authorized_hosts = %q, want newline-joined hosts", got)
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
	client := &recordingClient{files: make(map[string][]byte)}

	if err := create(context.Background(), cfg, io.Discard, io.Discard, func(context.Context) (imageClient, error) {
		return client, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := client.files["/root/assets/authorized_hosts"]; len(got) != 0 {
		t.Errorf("authorized_hosts = %q, want empty file", got)
	}
}

func TestCreateUsesNonCanceledBoundedContextForCleanup(t *testing.T) {
	cfg := imageConfig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	client := &recordingClient{
		files: make(map[string][]byte),
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
	if client.cleanupContextErr != nil {
		t.Errorf("cleanup context error = %v, want non-canceled context", client.cleanupContextErr)
	}
	if client.cleanupDeadlineRemaining <= 0 || client.cleanupDeadlineRemaining > 30*time.Second {
		t.Errorf("cleanup deadline remaining = %v, want bounded by 30s", client.cleanupDeadlineRemaining)
	}
	if got := client.calls[len(client.calls)-1]; got != "cleanup-delete-instance" {
		t.Errorf("last call = %q, want cleanup-delete-instance", got)
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
		"cobalt-ember.json": "theme",
		"tmux.conf":         "tmux",
	} {
		if err := os.WriteFile(filepath.Join(assetDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return config.Config{
		Dir: dir,
		BaseImage: config.BaseImage{
			Name:            "sandbox",
			Source:          "https://images.linuxcontainers.org",
			Image:           "debian/13",
			AuthorizedHosts: hosts,
		},
	}
}

type recordingClient struct {
	calls                    []string
	files                    map[string][]byte
	profileDefinition        []byte
	createRequest            api.InstancesPost
	publishAlias             string
	publishDescription       string
	exec                     func() error
	cleanupContextErr        error
	cleanupDeadlineRemaining time.Duration
}

func (c *recordingClient) EnsureProfile(_ context.Context, name string, definition []byte) error {
	c.calls = append(c.calls, "ensure-profile "+name)
	c.profileDefinition = append([]byte(nil), definition...)
	return nil
}

func (c *recordingClient) CreateInstance(_ context.Context, request api.InstancesPost) error {
	c.calls = append(c.calls, "create-instance")
	c.createRequest = request
	return nil
}

func (c *recordingClient) PushFile(_ context.Context, _ string, path string, content []byte, _ int) error {
	c.calls = append(c.calls, "push "+path)
	c.files[path] = append([]byte(nil), content...)
	return nil
}

func (c *recordingClient) Exec(_ context.Context, _ string, request incusclient.ExecRequest) (string, string, error) {
	c.calls = append(c.calls, "exec "+strings.Join(request.Command, " "))
	if c.exec != nil {
		return "", "", c.exec()
	}
	return "installer output", "", nil
}

func (c *recordingClient) StopInstance(_ context.Context, _ string, _ bool) error {
	c.calls = append(c.calls, "stop")
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
	c.cleanupContextErr = ctx.Err()
	deadline, ok := ctx.Deadline()
	if ok {
		c.cleanupDeadlineRemaining = time.Until(deadline)
	}
	return nil
}

func (c *recordingClient) Disconnect() {}
