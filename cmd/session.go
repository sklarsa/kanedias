package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
)

type SessionOptions struct {
	SocketPath string
	ConfigPath string
}

func newSessionCommand(service services, configPath func() string) *cobra.Command {
	var socketPath string
	command := &cobra.Command{
		Use:   "session",
		Short: "Run a foreground supervised Pi session",
		Args:  cobra.NoArgs,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if socketPath == "" {
				return fmt.Errorf("--socket is required")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			selectedConfig := configPath()
			absoluteConfig, err := filepath.Abs(selectedConfig)
			if err != nil {
				return fmt.Errorf("resolve config path %q: %w", selectedConfig, err)
			}
			cfg, err := service.loadConfig(selectedConfig)
			if err != nil {
				return err
			}
			return service.runSupervisor(cmd.Context(), cfg, SessionOptions{
				SocketPath: socketPath,
				ConfigPath: absoluteConfig,
			}, cmd.OutOrStdout())
		},
	}
	command.Flags().StringVar(&socketPath, "socket", "", "host Unix socket path for this root supervisor")
	return command
}
