package cmd

import "github.com/spf13/cobra"

func newWorkspaceCommand(service services, configPath func() string) *cobra.Command {
	command := &cobra.Command{
		Use:   "workspace",
		Short: "Manage the Incus workspace",
	}
	command.AddCommand(
		newWorkspaceIncusCommand(service, configPath),
		newWorkspaceReposCommand(service, configPath),
	)
	return command
}

func newWorkspaceIncusCommand(service services, configPath func() string) *cobra.Command {
	command := &cobra.Command{
		Use:   "incus",
		Short: "Manage Incus workspace state",
	}
	command.AddCommand(newWorkspaceIncusSyncCommand(service, configPath))
	return command
}

func newWorkspaceIncusSyncCommand(service services, configPath func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Synchronize Incus workspace state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := service.loadConfig(configPath())
			if err != nil {
				return err
			}
			return service.syncIncusWorkspace(cmd.Context(), cfg, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

func newWorkspaceReposCommand(service services, configPath func() string) *cobra.Command {
	command := &cobra.Command{
		Use:   "repos",
		Short: "Manage workspace repositories",
	}
	command.AddCommand(newWorkspaceReposSyncCommand(service, configPath))
	return command
}

func newWorkspaceReposSyncCommand(service services, configPath func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Synchronize workspace repositories",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := service.loadConfig(configPath())
			if err != nil {
				return err
			}
			return service.syncRepos(cmd.Context(), cfg, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}
