package scenario

import (
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

// Frame is a request frame under construction.
type Frame = *aiserverv1.GrokBotUserComputerRequestFrame

// execID is the exec ID every exec request carries; the answers repeat it.
const execID = 7

// ExecJSON is an exec request with the given agent.v1.ExecServerMessage JSON.
func ExecJSON(id, serverMessageJSON string) Frame {
	return &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{
		Exec: &aiserverv1.GrokBotUserComputerExec{ServerMessageJson: serverMessageJSON},
	}}
}

func exec(id string, m *agentv1.ExecServerMessage) Frame {
	m.Id = execID
	return ExecJSON(id, compact(m))
}

// Shell is a shell command with default arguments.
func Shell(id, command string) Frame { return ShellArgs(id, &agentv1.ShellArgs{Command: command}) }

// ShellArgs is a shell command.
func ShellArgs(id string, args *agentv1.ShellArgs) Frame {
	return exec(id, &agentv1.ExecServerMessage{Message: &agentv1.ExecServerMessage_ShellStreamArgs{ShellStreamArgs: args}})
}

// Background starts a background shell command.
func Background(id string, args *agentv1.BackgroundShellSpawnArgs) Frame {
	return exec(id, &agentv1.ExecServerMessage{
		Message: &agentv1.ExecServerMessage_BackgroundShellSpawnArgs{BackgroundShellSpawnArgs: args},
	})
}

// Read reads a file with default arguments.
func Read(id, path string) Frame { return ReadArgs(id, &agentv1.ReadArgs{Path: path}) }

// ReadArgs reads a file.
func ReadArgs(id string, args *agentv1.ReadArgs) Frame {
	return exec(id, &agentv1.ExecServerMessage{Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: args}})
}

// Ls lists a directory with default arguments.
func Ls(id, path string) Frame { return LsArgs(id, &agentv1.LsArgs{Path: path}) }

// LsArgs lists a directory.
func LsArgs(id string, args *agentv1.LsArgs) Frame {
	return exec(id, &agentv1.ExecServerMessage{Message: &agentv1.ExecServerMessage_LsArgs{LsArgs: args}})
}

// Upload writes data to path.
func Upload(id, path string, data []byte) Frame {
	return &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Upload{
		Upload: &aiserverv1.GrokBotUserComputerUpload{Path: path, Data: data},
	}}
}

// Download reads path.
func Download(id, path string) Frame {
	return &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Download{
		Download: &aiserverv1.GrokBotUserComputerDownload{Path: path},
	}}
}

// Discover asks which AI tool setup the computer has for source.
func Discover(id, source string) Frame {
	return &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_DiscoverAiTool{
		DiscoverAiTool: &aiserverv1.GrokBotUserComputerDiscoverAiTool{Source: source},
	}}
}

// Cancel cancels the request with that ID.
func Cancel(id string) Frame {
	return &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Cancel{
		Cancel: &aiserverv1.GrokBotUserComputerCancel{},
	}}
}

// Authorization is how the backend says a request is allowed.
type Authorization struct {
	ApprovalID string
	ByStanding bool
	ByApproval bool
}

// With returns a copy of f carrying the authorization.
func With(f Frame, a Authorization) Frame {
	f = proto.CloneOf(f)
	var approval *string
	if a.ApprovalID != "" {
		approval = &a.ApprovalID
	}
	switch x := f.GetFrame().(type) {
	case *aiserverv1.GrokBotUserComputerRequestFrame_Exec:
		x.Exec.ApprovalId, x.Exec.AuthorizedByStanding, x.Exec.AuthorizedByApproval = approval, a.ByStanding, a.ByApproval
	case *aiserverv1.GrokBotUserComputerRequestFrame_Upload:
		x.Upload.ApprovalId, x.Upload.AuthorizedByStanding, x.Upload.AuthorizedByApproval = approval, a.ByStanding, a.ByApproval
	case *aiserverv1.GrokBotUserComputerRequestFrame_Download:
		x.Download.ApprovalId, x.Download.AuthorizedByStanding, x.Download.AuthorizedByApproval = approval, a.ByStanding, a.ByApproval
	case *aiserverv1.GrokBotUserComputerRequestFrame_DiscoverAiTool:
		x.DiscoverAiTool.ApprovalId, x.DiscoverAiTool.AuthorizedByStanding = approval, a.ByStanding
		x.DiscoverAiTool.AuthorizedByApproval = a.ByApproval
	default:
		panic("scenario: frame takes no authorization")
	}
	return f
}
