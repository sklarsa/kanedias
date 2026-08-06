package cmd

import (
	"context"
	"io"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/network"
	"github.com/sklarsa/kanedias/internal/profiles"
	"github.com/sklarsa/kanedias/internal/proxy"
	"github.com/spf13/cobra"
)

type services struct {
	loadConfig       func(string) (config.Config, error)
	ensureNetwork    func(context.Context, config.Config) error
	renderProfile    func(io.Writer, string, config.Config) error
	runProxy         func(proxy.Options) error
	initCA           func(string, string) error
	loginOpenAICodex func(context.Context, string, io.Writer) error
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
	}
}

func newRootCommand(service services, options proxy.Options) *cobra.Command {
	configPath := "./config.toml"
	root := &cobra.Command{
		Use:          "kanedias",
		Short:        "Manage Kanedias Incus profiles and proxy",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", configPath, "path to the Kanedias configuration file")
	getConfigPath := func() string { return configPath }
	root.AddCommand(
		newProfileCommand(service, getConfigPath),
		newProxyCommand(service, getConfigPath, options),
	)
	return root
}
