package read

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/nodeerr"
)

// videoExtensions are returned as data when the file is binary (YO).
var videoExtensions = []string{".mp4", ".webm", ".mov", ".avi", ".mkv", ".wmv", ".flv", ".m4v"}

// Messages shown to the agent, matching the desktop (dut, Kpe, X_).
const (
	msgBinaryFile = "File content is binary data, not text"
	msgDirectory  = "Path is a directory, not a file"
	msgNotFile    = "Path is neither a file nor a directory"
	msgEmptyPath  = "path is empty or only whitespace"
)

// isBlank reports a path of JavaScript whitespace only (trim).
func isBlank(s string) bool {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
			return true
		}
		return r >= 0x2000 && r <= 0x200a
	}) == ""
}

// extname is Node's path.extname, lowercased: a dotfile such as ".pdf" has
// no extension.
func extname(path string) string {
	name := filepath.Base(path)
	if i := strings.LastIndexByte(name, '.'); i > 0 && name != ".." {
		return strings.ToLower(name[i:])
	}
	return ""
}

// isVideo matches a video extension after the last dot of the path, as the
// desktop does (zKt), dotfiles included.
func isVideo(path string) bool {
	path = strings.ToLower(path)
	i := strings.LastIndexByte(path, '.')
	return i >= 0 && slices.Contains(videoExtensions, path[i:])
}

func readMessage(r *agentv1.ReadResult) *agentv1.ExecClientMessage {
	return &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ReadResult{ReadResult: r}}
}

// readError is the result for a failed read: missing, denied, or the error
// in Node's words, as the desktop passes it on.
func readError(path string, err error) *agentv1.ReadResult {
	r := &agentv1.ReadResult{}
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.Result = &agentv1.ReadResult_FileNotFound{FileNotFound: &agentv1.ReadFileNotFound{Path: path}}
	case errors.Is(err, os.ErrPermission):
		r.Result = &agentv1.ReadResult_PermissionDenied{PermissionDenied: &agentv1.ReadPermissionDenied{Path: path}}
	default:
		r.Result = &agentv1.ReadResult_Error{Error: &agentv1.ReadError{Path: path, Error: nodeerr.Wrap(err, path).Error()}}
	}
	return r
}

func invalidFile(path, reason string) *agentv1.ReadResult {
	return &agentv1.ReadResult{Result: &agentv1.ReadResult_InvalidFile{
		InvalidFile: &agentv1.ReadInvalidFile{Path: path, Reason: reason},
	}}
}
