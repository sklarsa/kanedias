package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor/process"
	"github.com/spf13/cobra"
)

type SessionOptions struct {
	SocketPath string
	ConfigPath string
	Policy     config.SessionModelPolicy
}

func newSessionCommand(service services, configPath func() string) *cobra.Command {
	var socketPath string
	bootstrapFD := -1
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

			var policy config.SessionModelPolicy
			if cmd.Flags().Changed("bootstrap-fd") {
				if bootstrapFD < 0 {
					return fmt.Errorf("--bootstrap-fd must be a non-negative descriptor")
				}
				syscall.CloseOnExec(bootstrapFD)
				bootstrapFile := os.NewFile(uintptr(bootstrapFD), "root-bootstrap")
				if bootstrapFile == nil {
					return fmt.Errorf("open inherited root bootstrap descriptor %d", bootstrapFD)
				}
				bootstrap, decodeErr := process.DecodeRootBootstrap(bootstrapFile)
				closeErr := bootstrapFile.Close()
				if decodeErr != nil || closeErr != nil {
					return errors.Join(decodeErr, closeErr)
				}
				policy = bootstrap.Policy
			} else {
				policy, err = cfg.DefaultSessionModelPolicy()
				if err != nil {
					return fmt.Errorf("resolve default session model policy: %w", err)
				}
			}
			return service.runSupervisor(cmd.Context(), cfg, SessionOptions{
				SocketPath: socketPath,
				ConfigPath: absoluteConfig,
				Policy:     policy,
			}, cmd.OutOrStdout())
		},
	}
	command.Flags().StringVar(&socketPath, "socket", "", "host Unix socket path for this root supervisor")
	command.Flags().IntVar(&bootstrapFD, "bootstrap-fd", -1, "inherited root-bootstrap descriptor")
	_ = command.Flags().MarkHidden("bootstrap-fd")
	return command
}
