package image

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
	"github.com/sklarsa/kanedias/internal/profiles"
)

const cleanupTimeout = 30 * time.Second

//go:embed install.sh
var installer []byte

//go:embed kanedias-pi.socket
var piRPCSocket []byte

//go:embed kanedias-pi@.service
var piRPCService []byte

//go:embed kanedias-pi-rpc
var piRPCLauncher []byte

type imageClient interface {
	EnsureProfile(context.Context, string, []byte) error
	CreateInstance(context.Context, api.InstancesPost) error
	PushFile(context.Context, string, string, []byte, int) error
	Exec(context.Context, string, incusclient.ExecRequest) (string, string, error)
	StopInstance(context.Context, string, bool) error
	PublishInstance(context.Context, string, string, string) error
	DeleteInstance(context.Context, string) error
	Disconnect()
}

type connector func(context.Context) (imageClient, error)

type buildInputs struct {
	piSettings []byte
	piTheme    []byte
	tmuxConfig []byte
	profile    []byte
}

// Create builds and publishes the configured base image through Incus.
func Create(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) error {
	return create(ctx, cfg, stdout, stderr, func(ctx context.Context) (imageClient, error) {
		return incusclient.Connect(ctx)
	})
}

func create(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, connect connector) error {
	if err := cfg.ValidateLifecycle(); err != nil {
		return err
	}

	inputs, err := loadBuildInputs(cfg)
	if err != nil {
		return err
	}

	client, err := connect(ctx)
	if err != nil {
		return err
	}
	defer client.Disconnect()

	return createWithClient(ctx, client, cfg, inputs, stdout, stderr)
}

func loadBuildInputs(cfg config.Config) (buildInputs, error) {
	var inputs buildInputs
	assets := []struct {
		name        string
		destination *[]byte
	}{
		{name: "pi-settings.json", destination: &inputs.piSettings},
		{name: "cobalt-ember.json", destination: &inputs.piTheme},
		{name: "tmux.conf", destination: &inputs.tmuxConfig},
	}
	for _, asset := range assets {
		contents, err := os.ReadFile(cfg.AssetPath(asset.name))
		if err != nil {
			return buildInputs{}, fmt.Errorf("read image asset %q: %w", cfg.AssetPath(asset.name), err)
		}
		*asset.destination = contents
	}

	var profile bytes.Buffer
	if err := profiles.Render(&profile, string(profiles.ImageBuild), cfg); err != nil {
		return buildInputs{}, fmt.Errorf("render image-build profile: %w", err)
	}
	inputs.profile = profile.Bytes()
	return inputs, nil
}

func createWithClient(ctx context.Context, client imageClient, cfg config.Config, inputs buildInputs, stdout, stderr io.Writer) (err error) {
	fmt.Fprintln(stdout, "Ensuring image-build profile...")
	if err := client.EnsureProfile(ctx, string(profiles.ImageBuild), inputs.profile); err != nil {
		return fmt.Errorf("ensure image-build profile: %w", err)
	}

	instanceName := fmt.Sprintf("image-build-%d-%d", time.Now().UnixNano(), os.Getpid())
	fmt.Fprintf(stdout, "Creating temporary instance %s...\n", instanceName)
	request := api.InstancesPost{
		InstancePut: api.InstancePut{Profiles: []string{"default", string(profiles.ImageBuild)}},
		Name:        instanceName,
		Source: api.InstanceSource{
			Type:     "image",
			Alias:    cfg.BaseImage.Image,
			Server:   cfg.BaseImage.Source,
			Protocol: "simplestreams",
		},
		Type:  api.InstanceTypeContainer,
		Start: true,
	}
	if err := client.CreateInstance(ctx, request); err != nil {
		return fmt.Errorf("create temporary image-build instance: %w", err)
	}
	instanceRunning := true
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		var cleanupErr error
		if instanceRunning {
			if stopErr := client.StopInstance(cleanupCtx, instanceName, true); stopErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("stop temporary image-build instance %q: %w", instanceName, stopErr))
			}
		}
		if deleteErr := client.DeleteInstance(cleanupCtx, instanceName); deleteErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete temporary image-build instance %q: %w", instanceName, deleteErr))
		}
		err = errors.Join(err, cleanupErr)
	}()

	fmt.Fprintln(stdout, "Uploading image build inputs...")
	if err := client.PushFile(ctx, instanceName, "/root/install.sh", installer, 0o700); err != nil {
		return fmt.Errorf("upload image build input %q: %w", "/root/install.sh", err)
	}
	if _, _, err := client.Exec(ctx, instanceName, incusclient.ExecRequest{
		Command: []string{"install", "-d", "/root/assets"},
	}); err != nil {
		return fmt.Errorf("create image asset directory: %w", err)
	}
	files := []struct {
		path    string
		content []byte
		mode    int
	}{
		{path: "/root/assets/authorized_hosts", content: []byte(strings.Join(cfg.BaseImage.AuthorizedHosts, "\n")), mode: 0o600},
		{path: "/root/assets/pi-settings.json", content: inputs.piSettings, mode: 0o644},
		{path: "/root/assets/cobalt-ember.json", content: inputs.piTheme, mode: 0o644},
		{path: "/root/assets/tmux.conf", content: inputs.tmuxConfig, mode: 0o644},
		{path: "/root/assets/kanedias-pi.socket", content: piRPCSocket, mode: 0o644},
		{path: "/root/assets/kanedias-pi@.service", content: piRPCService, mode: 0o644},
		{path: "/root/assets/kanedias-pi-rpc", content: piRPCLauncher, mode: 0o700},
	}
	for _, file := range files {
		if err := client.PushFile(ctx, instanceName, file.path, file.content, file.mode); err != nil {
			return fmt.Errorf("upload image build input %q: %w", file.path, err)
		}
	}

	fmt.Fprintln(stdout, "Running image installer...")
	execStdout, execStderr, execErr := client.Exec(ctx, instanceName, incusclient.ExecRequest{
		Command: []string{"bash", "/root/install.sh"},
	})
	_, _ = io.WriteString(stdout, execStdout)
	_, _ = io.WriteString(stderr, execStderr)
	if execErr != nil {
		return fmt.Errorf("run image installer: %w", execErr)
	}

	fmt.Fprintf(stdout, "Stopping temporary instance %s...\n", instanceName)
	if err := client.StopInstance(ctx, instanceName, false); err != nil {
		return fmt.Errorf("stop temporary image-build instance: %w", err)
	}
	instanceRunning = false

	description := fmt.Sprintf("kanedias sandbox from %s/%s", cfg.BaseImage.Source, cfg.BaseImage.Image)
	fmt.Fprintf(stdout, "Publishing image %s...\n", cfg.BaseImage.Name)
	if err := client.PublishInstance(ctx, instanceName, cfg.BaseImage.Name, description); err != nil {
		return fmt.Errorf("publish image %q: %w", cfg.BaseImage.Name, err)
	}
	fmt.Fprintf(stdout, "Published image %s.\n", cfg.BaseImage.Name)
	return nil
}
