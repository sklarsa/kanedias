package cmd

import (
	"github.com/sklarsa/kanedias/internal/supervisor/process"
	"github.com/spf13/cobra"
)

func newSessionChildCommand(runner process.ChildRunner) *cobra.Command {
	bootstrapFD := process.BootstrapFD
	livenessFD := process.LivenessFD
	reportFD := process.ReportFD
	terminalAckFD := process.TerminalAckFD
	command := &cobra.Command{
		Use:    "session-child",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return process.RunInheritedChild(command.Context(), bootstrapFD, livenessFD, reportFD, terminalAckFD, runner)
		},
	}
	command.Flags().IntVar(&bootstrapFD, "bootstrap-fd", process.BootstrapFD, "inherited bootstrap descriptor")
	command.Flags().IntVar(&livenessFD, "liveness-fd", process.LivenessFD, "inherited parent-liveness descriptor")
	command.Flags().IntVar(&reportFD, "report-fd", process.ReportFD, "inherited child-report descriptor")
	command.Flags().IntVar(&terminalAckFD, "terminal-ack-fd", process.TerminalAckFD, "inherited parent terminal-ack descriptor")
	return command
}
