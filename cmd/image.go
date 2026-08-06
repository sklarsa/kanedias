package cmd

import "github.com/spf13/cobra"

func newImageCommand(service services, configPath func() string) *cobra.Command {
	command := &cobra.Command{
		Use:   "image",
		Short: "Manage the Incus image",
	}
	command.AddCommand(newImageCreateCommand(service, configPath))
	return command
}

func newImageCreateCommand(service services, configPath func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "create",
		Short: "Create the Incus image",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := service.loadConfig(configPath())
			if err != nil {
				return err
			}
			return service.createImage(cmd.Context(), cfg, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}
