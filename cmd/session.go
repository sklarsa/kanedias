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
		RunE: func(cmd *cobra.Command, _ []string) error {
			inheritedBootstrap := cmd.Flags().Changed("bootstrap-fd")
			var policy config.SessionModelPolicy
			if inheritedBootstrap {
				if bootstrapFD < process.RootBootstrapFD {
					return fmt.Errorf("--bootstrap-fd must be at least %d", process.RootBootstrapFD)
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
			}
			if socketPath == "" {
				return fmt.Errorf("--socket is required")
			}

			selectedConfig := configPath()
			absoluteConfig, err := filepath.Abs(selectedConfig)
			if err != nil {
				return fmt.Errorf("resolve config path %q: %w", selectedConfig, err)
			}
			cfg, err := service.loadConfig(selectedConfig)
			if err != nil {
				return err
			}
			if !inheritedBootstrap {
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
