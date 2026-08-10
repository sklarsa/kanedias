package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor/process"
	"github.com/spf13/cobra"
)

type SessionOptions struct {
	SocketPath string
	ConfigPath string
	Policy     config.SessionModelPolicy
	Workspace  config.WorkspaceStart
	RootStatus io.WriteCloser
}

type onceFile struct {
	*os.File
	once sync.Once
	err  error
}

func (file *onceFile) Close() error {
	file.once.Do(func() { file.err = file.File.Close() })
	return file.err
}

func openSessionInheritedFile(descriptor int, name string) *onceFile {
	syscall.CloseOnExec(descriptor)
	file := os.NewFile(uintptr(descriptor), name)
	if file == nil {
		return nil
	}
	return &onceFile{File: file}
}

func newSessionCommand(service services, configPath func() string) *cobra.Command {
	var socketPath string
	bootstrapFD := -1
	statusFD := -1
	command := &cobra.Command{
		Use:   "session",
		Short: "Run a foreground supervised Pi session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			inheritedBootstrap := cmd.Flags().Changed("bootstrap-fd")
			inheritedStatus := cmd.Flags().Changed("status-fd")

			// Own every supplied non-stdio descriptor before validating the pair,
			// so a bad counterpart cannot leak an otherwise valid endpoint. Equal
			// descriptors share one idempotently closed file object.
			owned := make(map[int]*onceFile, 2)
			own := func(descriptor int, name string) *onceFile {
				if descriptor < process.RootBootstrapFD {
					return nil
				}
				if file := owned[descriptor]; file != nil {
					return file
				}
				file := openSessionInheritedFile(descriptor, name)
				if file != nil {
					owned[descriptor] = file
				}
				return file
			}
			var bootstrapFile, rootStatus *onceFile
			if inheritedBootstrap {
				bootstrapFile = own(bootstrapFD, "root-bootstrap")
				if bootstrapFile != nil {
					defer func() { _ = bootstrapFile.Close() }()
				}
			}
			if inheritedStatus {
				rootStatus = own(statusFD, "root-status")
				if rootStatus != nil {
					defer func() { _ = rootStatus.Close() }()
				}
			}

			if inheritedBootstrap && bootstrapFD < process.RootBootstrapFD {
				return fmt.Errorf("--bootstrap-fd must be at least %d", process.RootBootstrapFD)
			}
			if inheritedStatus && statusFD < process.RootStatusFD {
				return fmt.Errorf("--status-fd must be at least %d", process.RootStatusFD)
			}
			if inheritedBootstrap && inheritedStatus && bootstrapFD == statusFD {
				return fmt.Errorf("--bootstrap-fd and --status-fd must be distinct")
			}
			if inheritedStatus && rootStatus == nil {
				return fmt.Errorf("open inherited root status descriptor %d", statusFD)
			}

			var policy config.SessionModelPolicy
			var workspace config.WorkspaceStart
			if inheritedBootstrap {
				if bootstrapFile == nil {
					return fmt.Errorf("open inherited root bootstrap descriptor %d", bootstrapFD)
				}
				bootstrap, decodeErr := process.DecodeRootBootstrap(bootstrapFile)
				closeErr := bootstrapFile.Close()
				if decodeErr != nil || closeErr != nil {
					return errors.Join(decodeErr, closeErr)
				}
				policy = bootstrap.Policy
				workspace = bootstrap.Workspace
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
			var rootStatusWriter io.WriteCloser
			if rootStatus != nil {
				rootStatusWriter = rootStatus
			}
			return service.runSupervisor(cmd.Context(), cfg, SessionOptions{
				SocketPath: socketPath,
				ConfigPath: absoluteConfig,
				Policy:     policy,
				Workspace:  workspace,
				RootStatus: rootStatusWriter,
			}, cmd.OutOrStdout())
		},
	}
	command.Flags().StringVar(&socketPath, "socket", "", "host Unix socket path for this root supervisor")
	command.Flags().IntVar(&bootstrapFD, "bootstrap-fd", -1, "inherited root-bootstrap descriptor")
	command.Flags().IntVar(&statusFD, "status-fd", -1, "inherited root startup-status descriptor")
	_ = command.Flags().MarkHidden("bootstrap-fd")
	_ = command.Flags().MarkHidden("status-fd")
	return command
}
