package provision

import (
	"context"
	"fmt"
	"strings"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
)

const managedUser = "kanedias"

type instanceExecClient interface {
	Exec(context.Context, string, incusclient.ExecRequest) (string, string, error)
}

// prepareSessionWorkspace enforces durable ownership and mode on a cloned
// workspace's mounted seed. Repository checkout validation is completed in a
// later task; for now every start performs the same default-root ownership
// repair so cloned sessions never surface as root-owned and immutable.
func prepareSessionWorkspace(ctx context.Context, client instanceExecClient, instance string, start config.WorkspaceStart) error {
	commands := [][]string{
		{"chown", managedUser + ":" + managedUser, config.WorkspaceRoot},
		{"chmod", "0755", config.WorkspaceRoot},
		{"install", "-d", "-o", managedUser, "-g", managedUser, "-m", "0755", config.WorkspaceRepositoriesRoot},
		{"chown", managedUser + ":" + managedUser, config.WorkspaceRepositoriesRoot},
		{"chmod", "0755", config.WorkspaceRepositoriesRoot},
	}
	for _, command := range commands {
		if _, _, err := client.Exec(ctx, instance, incusclient.ExecRequest{Command: command}); err != nil {
			return fmt.Errorf("prepare session workspace: execute %q: %w", strings.Join(command, " "), err)
		}
	}
	return nil
}
