package read

import (
	"errors"
	"os"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
)

// binaryExtensions are returned as data instead of text.
var binaryExtensions = []string{".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp"}

// Messages shown to the agent, matching the desktop (dut, Kpe).
const (
	msgBinaryFile = "File content is binary data, not text"
	msgDirectory  = "Path is a directory, not a file"
	msgEmptyPath  = "path is empty or only whitespace"
)

func readMessage(r *agentv1.ReadResult) *agentv1.ExecClientMessage {
	return &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ReadResult{ReadResult: r}}
}

func readError(path string, err error) *agentv1.ReadResult {
	r := &agentv1.ReadResult{}
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.Result = &agentv1.ReadResult_FileNotFound{FileNotFound: &agentv1.ReadFileNotFound{Path: path}}
	case errors.Is(err, os.ErrPermission):
		r.Result = &agentv1.ReadResult_PermissionDenied{PermissionDenied: &agentv1.ReadPermissionDenied{Path: path}}
	default:
		r.Result = &agentv1.ReadResult_Error{Error: &agentv1.ReadError{Path: path, Error: err.Error()}}
	}
	return r
}

func invalidFile(path, reason string) *agentv1.ReadResult {
	return &agentv1.ReadResult{Result: &agentv1.ReadResult_InvalidFile{
		InvalidFile: &agentv1.ReadInvalidFile{Path: path, Reason: reason},
	}}
}
