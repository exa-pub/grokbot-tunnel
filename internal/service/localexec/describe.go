package localexec

import (
	"fmt"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
)

// describe returns a human-readable action for the permission prompt, or ""
// if the exec request is not supported. Every server-supplied string is
// quoted, so control characters cannot rewrite what the prompt shows.
func describe(msg *agentv1.ExecServerMessage) string {
	switch {
	case msg.GetShellStreamArgs() != nil:
		x := msg.GetShellStreamArgs()
		return fmt.Sprintf("Run %q in %q", x.Command, x.WorkingDirectory)
	case msg.GetBackgroundShellSpawnArgs() != nil:
		x := msg.GetBackgroundShellSpawnArgs()
		return fmt.Sprintf("Run background %q in %q", x.Command, x.WorkingDirectory)
	case msg.GetReadArgs() != nil:
		return fmt.Sprintf("Read %q", msg.GetReadArgs().Path)
	case msg.GetLsArgs() != nil:
		return fmt.Sprintf("List %q", msg.GetLsArgs().Path)
	}
	return ""
}
