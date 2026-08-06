package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

func newSessionCommand(service services, configPath func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "session",
		Short: "Run one prompt in an ephemeral Pi sandbox",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			prompt, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return fmt.Errorf("read session prompt: %w", err)
			}
			if strings.TrimSpace(string(prompt)) == "" {
				return fmt.Errorf("session prompt on stdin is empty")
			}
			cfg, err := service.loadConfig(configPath())
			if err != nil {
				return err
			}
			return service.runSession(
				cmd.Context(), cfg, string(prompt),
				cmd.OutOrStdout(), cmd.ErrOrStderr(),
			)
		},
	}
}
