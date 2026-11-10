package frames

import (
	"errors"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

// AI tool discovery sources the desktop understands.
const (
	SourceClaudeCode = "claude_code"
	SourceCodex      = "codex"
)

// ErrUnknownFrame answers a request frame the desktop does not parse, in its
// words.
//
//nolint:revive,staticcheck // user-facing sentence
var ErrUnknownFrame = errors.New("This computer's local-exec daemon does not understand that request frame; update the desktop app.")

// Unrecognized reports whether f is a request the desktop answers with
// ErrUnknownFrame before scheduling it: a frame of no known type, a URL
// transfer (put/get file to/from URL, which neither advertises), AI tool
// discovery for an unknown source or a Messages operation it cannot parse.
func Unrecognized(f *aiserverv1.GrokBotUserComputerRequestFrame) bool {
	switch v := f.Frame.(type) {
	case nil, *aiserverv1.GrokBotUserComputerRequestFrame_PutFileToUrl, *aiserverv1.GrokBotUserComputerRequestFrame_GetFileFromUrl:
		return true
	case *aiserverv1.GrokBotUserComputerRequestFrame_DiscoverAiTool:
		return v.DiscoverAiTool.Source != SourceCodex && v.DiscoverAiTool.Source != SourceClaudeCode
	case *aiserverv1.GrokBotUserComputerRequestFrame_MessagesOp:
		_, ok := messagesOp(v.MessagesOp.GetOpJson())
		return !ok
	}
	return false
}
