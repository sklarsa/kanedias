package cmd

import (
	"net"

	"github.com/sklarsa/kanedias/internal/proxy"
	"github.com/spf13/cobra"
)

func newProxyCommand(service services, configPath func() string, defaults proxy.Options) *cobra.Command {
	command := &cobra.Command{
		Use:   "proxy",
		Short: "Manage the credential proxy",
	}
	command.AddCommand(
		newProxyRunCommand(service, configPath, defaults),
		newProxyInitCACommand(service, defaults),
		newProxyLoginCommand(service, defaults),
	)
	return command
}

func newProxyRunCommand(service services, configPath func() string, defaults proxy.Options) *cobra.Command {
	options := defaults
	command := &cobra.Command{
		Use:   "run",
		Short: "Ensure the Incus network and run the proxy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := service.loadConfig(configPath())
			if err != nil {
				return err
			}
			prefix, err := cfg.Network.IPv4Prefix()
			if err != nil {
				return err
			}
			options.ListenAddress = net.JoinHostPort(prefix.Addr().String(), "3128")
			if err := service.ensureNetwork(cmd.Context(), cfg); err != nil {
				return err
			}
			return service.runProxy(options)
		},
	}
	command.Flags().StringVar(&options.MetricsListenAddress, "metrics-listen", options.MetricsListenAddress, "address for the Prometheus metrics listener")
	command.Flags().BoolVar(&options.RequestLog, "request-log", options.RequestLog, "log proxy requests")
	command.Flags().StringVar(&options.CACertPath, "ca-cert", options.CACertPath, "path to the proxy CA certificate")
	command.Flags().StringVar(&options.CAKeyPath, "ca-key", options.CAKeyPath, "path to the proxy CA private key")
	command.Flags().StringVar(&options.ClaudeCredentialsPath, "claude-credentials", options.ClaudeCredentialsPath, "path to Claude credentials")
	command.Flags().StringVar(&options.OpenAICodexAuthPath, "openai-codex-auth", options.OpenAICodexAuthPath, "path to OpenAI Codex authentication")
	return command
}

func newProxyInitCACommand(service services, defaults proxy.Options) *cobra.Command {
	certPath := defaults.CACertPath
	keyPath := defaults.CAKeyPath
	command := &cobra.Command{
		Use:   "init-ca",
		Short: "Initialize the proxy certificate authority",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return service.initCA(certPath, keyPath)
		},
	}
	command.Flags().StringVar(&certPath, "ca-cert", certPath, "path to the proxy CA certificate")
	command.Flags().StringVar(&keyPath, "ca-key", keyPath, "path to the proxy CA private key")
	return command
}

func newProxyLoginCommand(service services, defaults proxy.Options) *cobra.Command {
	command := &cobra.Command{
		Use:   "login",
		Short: "Log in to a proxy credential provider",
	}
	command.AddCommand(newProxyLoginOpenAICodexCommand(service, defaults))
	return command
}

func newProxyLoginOpenAICodexCommand(service services, defaults proxy.Options) *cobra.Command {
	authPath := defaults.OpenAICodexAuthPath
	command := &cobra.Command{
		Use:   "openai-codex",
		Short: "Log in to OpenAI Codex",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return service.loginOpenAICodex(cmd.Context(), authPath, cmd.OutOrStdout())
		},
	}
	command.Flags().StringVar(&authPath, "openai-codex-auth", authPath, "path to OpenAI Codex authentication")
	return command
}
