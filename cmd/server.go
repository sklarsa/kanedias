package cmd

import (
	"log/slog"

	"github.com/sklarsa/kanedias/internal/server"
	"github.com/spf13/cobra"
)

func newServerCommand(service services) *cobra.Command {
	listenAddress := server.DefaultListenAddress
	command := &cobra.Command{
		Use:   "server",
		Short: "Run the local Kanedias web UI",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := server.ValidateListenAddress(listenAddress); err != nil {
				return err
			}

			logger := slog.New(slog.NewTextHandler(command.ErrOrStderr(), nil))
			return service.runServer(
				command.Context(),
				server.Options{
					ListenAddress: listenAddress,
					Logger:        logger,
				},
			)
		},
	}
	command.Flags().StringVar(&listenAddress, "listen", listenAddress, "local address for the Kanedias web UI")
	return command
}
