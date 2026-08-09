package image

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
	"github.com/sklarsa/kanedias/internal/network"
	"github.com/sklarsa/kanedias/internal/profiles"
)

const cleanupTimeout = 30 * time.Second

//go:embed install.sh
var installer []byte

//go:embed kanedias-pi.socket
var piRPCSocket []byte

//go:embed kanedias-pi@.service
var piRPCService []byte

//go:embed kanedias-pi-env
var piEnvironmentBridge []byte

//go:embed kanedias-pi-rpc
var piRPCLauncher []byte

//go:embed pi-extension/package-lock.json pi-extension/package.json pi-extension/skills/*/SKILL.md pi-extension/src/*.ts
var piExtension embed.FS

var piExtensionFiles = []string{
	"pi-extension/package-lock.json",
	"pi-extension/package.json",
	"pi-extension/skills/delegate-session/SKILL.md",
	"pi-extension/skills/writer-handoff/SKILL.md",
	"pi-extension/src/fork.ts",
	"pi-extension/src/git-handoff.ts",
	"pi-extension/src/index.ts",
	"pi-extension/src/schemas.ts",
	"pi-extension/src/supervisor-client.ts",
	"pi-extension/src/types.ts",
}

type imageClient interface {
	ResolvePool(context.Context, string) (string, error)
	GetNetwork(context.Context, string) (*api.Network, error)
	CreateNetwork(context.Context, api.NetworksPost) error
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

type buildScript struct {
	name    string
	content []byte
}

type buildInputs struct {
	piSettings []byte
	piAuth     []byte
	piModels   []byte
	profile    []byte
	scripts    []buildScript
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
		{name: "pi-auth.json", destination: &inputs.piAuth},
		{name: "pi-models.json", destination: &inputs.piModels},
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

	scripts, err := loadBuildScripts(cfg)
	if err != nil {
		return buildInputs{}, err
	}
	inputs.scripts = scripts
	return inputs, nil
}

func loadBuildScripts(cfg config.Config) ([]buildScript, error) {
	path := cfg.BuildScriptsPath()
	if path == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read image build scripts %q: %w", path, err)
	}

	var scripts []buildScript
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sh") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect image build script %q: %w", name, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			continue
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("image build script %q must be a regular file", name)
		}
		content, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			return nil, fmt.Errorf("read image build script %q: %w", name, err)
		}
		scripts = append(scripts, buildScript{name: name, content: content})
	}
	sort.Slice(scripts, func(i, j int) bool {
		return scripts[i].name < scripts[j].name
	})
	return scripts, nil
}

func createWithClient(ctx context.Context, client imageClient, cfg config.Config, inputs buildInputs, stdout, stderr io.Writer) (err error) {
	pool, err := client.ResolvePool(ctx, cfg.Workspace.Pool)
	if err != nil {
		return err
	}
	if err := network.EnsureWithClient(ctx, client, cfg); err != nil {
		return err
	}

	_, _ = fmt.Fprintln(stdout, "Ensuring image-build profile...")
	if err := client.EnsureProfile(ctx, string(profiles.ImageBuild), inputs.profile); err != nil {
		return fmt.Errorf("ensure image-build profile: %w", err)
	}

	instanceName := fmt.Sprintf("image-build-%d-%d", time.Now().UnixNano(), os.Getpid())
	_, _ = fmt.Fprintf(stdout, "Creating temporary instance %s...\n", instanceName)
	request := api.InstancesPost{
		InstancePut: api.InstancePut{
			Profiles: []string{"default", string(profiles.ImageBuild)},
			Devices: api.DevicesMap{
				"root": {
					"type": "disk",
					"pool": pool,
					"path": "/",
				},
			},
		},
		Name: instanceName,
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

	_, _ = fmt.Fprintln(stdout, "Uploading image build inputs...")
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
		{path: "/root/assets/pi-auth.json", content: inputs.piAuth, mode: 0o600},
		{path: "/root/assets/pi-models.json", content: inputs.piModels, mode: 0o644},
		{path: "/root/assets/kanedias-pi.socket", content: piRPCSocket, mode: 0o644},
		{path: "/root/assets/kanedias-pi@.service", content: piRPCService, mode: 0o644},
		{path: "/root/assets/kanedias-pi-env", content: piEnvironmentBridge, mode: 0o700},
		{path: "/root/assets/kanedias-pi-rpc", content: piRPCLauncher, mode: 0o700},
	}
	for _, file := range files {
		if err := client.PushFile(ctx, instanceName, file.path, file.content, file.mode); err != nil {
			return fmt.Errorf("upload image build input %q: %w", file.path, err)
		}
	}
	if _, _, err := client.Exec(ctx, instanceName, incusclient.ExecRequest{
		Command: []string{"install", "-d", "/root/assets/pi-extension/skills/delegate-session", "/root/assets/pi-extension/skills/writer-handoff", "/root/assets/pi-extension/src"},
	}); err != nil {
		return fmt.Errorf("create Pi extension asset directories: %w", err)
	}
	for _, embeddedPath := range piExtensionFiles {
		content, err := piExtension.ReadFile(embeddedPath)
		if err != nil {
			return fmt.Errorf("read embedded Pi extension file %q: %w", embeddedPath, err)
		}
		destination := "/root/assets/" + embeddedPath
		if err := client.PushFile(ctx, instanceName, destination, content, 0o644); err != nil {
			return fmt.Errorf("upload image build input %q: %w", destination, err)
		}
	}

	_, _ = fmt.Fprintln(stdout, "Running image installer...")
	if _, _, execErr := client.Exec(ctx, instanceName, incusclient.ExecRequest{
		Command: []string{"bash", "/root/install.sh"},
		Stdout:  stdout,
		Stderr:  stderr,
	}); execErr != nil {
		return fmt.Errorf("run image installer: %w", execErr)
	}
	if _, _, err := client.Exec(ctx, instanceName, incusclient.ExecRequest{
		Command: []string{"test", "-d", "/opt/kanedias/pi-extension/node_modules/typebox"},
	}); err != nil {
		return fmt.Errorf("verify Pi extension production dependencies: %w", err)
	}

	if len(inputs.scripts) > 0 {
		if _, _, err := client.Exec(ctx, instanceName, incusclient.ExecRequest{
			Command: []string{"install", "-d", "-m", "0700", "/root/build-scripts"},
		}); err != nil {
			return fmt.Errorf("create image build script directory: %w", err)
		}
		for _, script := range inputs.scripts {
			destination := "/root/build-scripts/" + script.name
			if err := client.PushFile(ctx, instanceName, destination, script.content, 0o700); err != nil {
				return fmt.Errorf("upload image build script %q: %w", script.name, err)
			}
		}
		for _, script := range inputs.scripts {
			_, _ = fmt.Fprintf(stdout, "Running image build script %s...\n", script.name)
			path := "/root/build-scripts/" + script.name
			if _, _, err := client.Exec(ctx, instanceName, incusclient.ExecRequest{
				Command: []string{path}, Stdout: stdout, Stderr: stderr,
			}); err != nil {
				return fmt.Errorf("run image build script %q: %w", script.name, err)
			}
		}
	}

	_, _ = fmt.Fprintf(stdout, "Stopping temporary instance %s...\n", instanceName)
	if err := client.StopInstance(ctx, instanceName, false); err != nil {
		return fmt.Errorf("stop temporary image-build instance: %w", err)
	}
	instanceRunning = false

	description := fmt.Sprintf("kanedias sandbox from %s/%s", cfg.BaseImage.Source, cfg.BaseImage.Image)
	_, _ = fmt.Fprintf(stdout, "Publishing image %s...\n", cfg.BaseImage.Name)
	if err := client.PublishInstance(ctx, instanceName, cfg.BaseImage.Name, description); err != nil {
		return fmt.Errorf("publish image %q: %w", cfg.BaseImage.Name, err)
	}
	_, _ = fmt.Fprintf(stdout, "Published image %s.\n", cfg.BaseImage.Name)
	return nil
}
