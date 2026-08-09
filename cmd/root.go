package cmd

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/image"
	"github.com/sklarsa/kanedias/internal/network"
	"github.com/sklarsa/kanedias/internal/profiles"
	"github.com/sklarsa/kanedias/internal/proxy"
	"github.com/sklarsa/kanedias/internal/server"
	"github.com/sklarsa/kanedias/internal/supervisor/process"
	"github.com/sklarsa/kanedias/internal/workspace"
	"github.com/spf13/cobra"
)

type services struct {
	loadConfig       func(string) (config.Config, error)
	ensureNetwork    func(context.Context, config.Config) error
	renderProfile    func(io.Writer, string, config.Config) error
	runProxy         func(context.Context, proxy.Options) error
	initCA           func(string, string) error
	loginOpenAICodex func(context.Context, string, io.Writer) error
	createImage      func(context.Context, config.Config, io.Writer, io.Writer) error
	runSupervisor    func(context.Context, config.Config, SessionOptions, io.Writer) error
	syncRepos        func(context.Context, config.Config, io.Writer, io.Writer) error
	runServer        func(context.Context, config.Config, server.Options) error
	runSessionChild  process.ChildRunner
}

// Execute runs the Kanedias command-line interface.
func Execute() error {
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()
	return ExecuteContext(ctx)
}

// ExecuteContext runs the Kanedias command-line interface with the supplied context.
func ExecuteContext(ctx context.Context) error {
	options, err := proxy.DefaultOptions()
	if err != nil {
		return err
	}
	return execute(ctx, realServices(), options)
}

func execute(ctx context.Context, service services, options proxy.Options) error {
	return newRootCommand(service, options).ExecuteContext(ctx)
}

func realServices() services {
	return services{
		loadConfig:       config.Load,
		ensureNetwork:    network.Ensure,
		renderProfile:    profiles.Render,
		runProxy:         proxy.RunContext,
		initCA:           proxy.InitCA,
		loginOpenAICodex: proxy.LoginOpenAICodex,
		createImage:      image.Create,
		runSupervisor:    runSupervisor,
		syncRepos:        workspace.Sync,
		runServer:        server.Run,
		runSessionChild:  productionChildRunner,
	}
}

func newRootCommand(service services, options proxy.Options) *cobra.Command {
	configPath := os.Getenv("KANEDIAS_CONFIG")
	if configPath == "" {
		configPath = "./config.toml"
	}
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
		newServerCommand(service, getConfigPath),
		newSessionChildCommand(service.runSessionChild),
		newSessionCommand(service, getConfigPath),
		newWorkspaceCommand(service, getConfigPath),
	)
	return root
}
