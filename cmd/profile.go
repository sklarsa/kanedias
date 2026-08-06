package cmd

import (
	"github.com/sklarsa/kanedias/internal/profiles"
	"github.com/spf13/cobra"
)

func newProfileCommand(service services, configPath func() string) *cobra.Command {
	validArgs := profiles.Types()
	return &cobra.Command{
		Use:       "profile <type>",
		Short:     "Render an Incus profile",
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgs: validArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := service.loadConfig(configPath())
			if err != nil {
				return err
			}
			return service.renderProfile(cmd.OutOrStdout(), args[0], cfg)
		},
	}
}
