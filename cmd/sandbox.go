package cmd

import "github.com/spf13/cobra"

const defaultSandboxName = "sandbox"

func newSandboxCommand(service services, configPath func() string) *cobra.Command {
	command := &cobra.Command{
		Use:   "sandbox",
		Short: "Manage Incus sandboxes",
	}
	command.AddCommand(
		newSandboxCreateCommand(service, configPath),
		newSandboxDestroyCommand(service, configPath),
	)
	return command
}

func newSandboxCreateCommand(service services, configPath func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "create [name]",
		Short: "Create an Incus sandbox",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := sandboxName(args)
			cfg, err := service.loadConfig(configPath())
			if err != nil {
				return err
			}
			return service.createSandbox(cmd.Context(), cfg, name, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

func newSandboxDestroyCommand(service services, configPath func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "destroy [name]",
		Short: "Destroy an Incus sandbox",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := sandboxName(args)
			cfg, err := service.loadConfig(configPath())
			if err != nil {
				return err
			}
			return service.destroySandbox(cmd.Context(), cfg, name, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

func sandboxName(args []string) string {
	if len(args) == 0 {
		return defaultSandboxName
	}
	return args[0]
}
