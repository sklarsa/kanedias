package cmd

import (
	"context"
	"io"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/image"
	"github.com/sklarsa/kanedias/internal/network"
	"github.com/sklarsa/kanedias/internal/profiles"
	"github.com/sklarsa/kanedias/internal/proxy"
	"github.com/sklarsa/kanedias/internal/sandbox"
	"github.com/sklarsa/kanedias/internal/session"
	"github.com/sklarsa/kanedias/internal/workspace"
	"github.com/spf13/cobra"
)

type services struct {
	loadConfig       func(string) (config.Config, error)
	ensureNetwork    func(context.Context, config.Config) error
	renderProfile    func(io.Writer, string, config.Config) error
	runProxy         func(proxy.Options) error
	initCA           func(string, string) error
	loginOpenAICodex func(context.Context, string, io.Writer) error
	createImage      func(context.Context, config.Config, io.Writer, io.Writer) error
	createSandbox    func(context.Context, config.Config, string, io.Writer, io.Writer) error
	destroySandbox   func(context.Context, config.Config, string, io.Writer, io.Writer) error
	runSession       func(context.Context, config.Config, string, io.Writer, io.Writer) error
	syncWorkspace    func(context.Context, config.Config, io.Writer, io.Writer) error
}

// Execute runs the Kanedias command-line interface.
func Execute() error {
	options, err := proxy.DefaultOptions()
	if err != nil {
		return err
	}
	return newRootCommand(realServices(), options).Execute()
}

func realServices() services {
	return services{
		loadConfig:       config.Load,
		ensureNetwork:    network.Ensure,
		renderProfile:    profiles.Render,
		runProxy:         proxy.Run,
		initCA:           proxy.InitCA,
		loginOpenAICodex: proxy.LoginOpenAICodex,
		createImage:      image.Create,
		createSandbox:    sandbox.Create,
		destroySandbox:   sandbox.Destroy,
		runSession:       session.Run,
		syncWorkspace:    workspace.Sync,
	}
}

func newRootCommand(service services, options proxy.Options) *cobra.Command {
	configPath := "./config.toml"
	root := &cobra.Command{
		Use:          "kanedias",
		Short:        "Incus lifecycle management for Kanedias profiles and proxy",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", configPath, "path to the Kanedias configuration file")
	getConfigPath := func() string { return configPath }
	root.AddCommand(
		newImageCommand(service, getConfigPath),
		newProfileCommand(service, getConfigPath),
		newProxyCommand(service, getConfigPath, options),
		newSandboxCommand(service, getConfigPath),
		newSessionCommand(service, getConfigPath),
		newWorkspaceCommand(service, getConfigPath),
	)
	return root
}
