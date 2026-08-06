package cmd

import "github.com/spf13/cobra"

func newWorkspaceCommand(service services, configPath func() string) *cobra.Command {
	command := &cobra.Command{
		Use:   "workspace",
		Short: "Manage the Incus workspace",
	}
	command.AddCommand(newWorkspaceSyncCommand(service, configPath))
	return command
}

func newWorkspaceSyncCommand(service services, configPath func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Synchronize the Incus workspace",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := service.loadConfig(configPath())
			if err != nil {
				return err
			}
			return service.syncWorkspace(cmd.Context(), cfg, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}
