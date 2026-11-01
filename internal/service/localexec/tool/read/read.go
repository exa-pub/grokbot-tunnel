// Package read reads a file for the agent, like the desktop's local-exec read
// executor (LocalReadExecutor): typed results for missing, unreadable and
// invalid files, line ranges, max_chars and text decoding.
package read

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
)

// Executor reads files below a root.
type Executor struct {
	root *pathroot.Root
}

// New returns an Executor for files below root.
func New(root *pathroot.Root) *Executor { return &Executor{root: root} }

// Execute reads args.Path and sends the read result. A path outside the root
// fails the request instead of producing a result, as on the desktop.
func (rd *Executor) Execute(ctx context.Context, x *agentv1.ReadArgs, send execstream.Send) error {
	if strings.TrimSpace(x.Path) == "" {
		return send(readMessage(&agentv1.ReadResult{Result: &agentv1.ReadResult_Error{
			Error: &agentv1.ReadError{Path: x.Path, Error: msgEmptyPath},
		}}))
	}
	rel, err := rd.root.Check(x.Path)
	if err != nil {
		return err
	}
	result, err := rd.result(ctx, rel, x)
	if err != nil {
		return err
	}
	return send(readMessage(result))
}

// result reads a file into a typed result. Only failures that the
// desktop reports as errors rather than results are returned as errors.
func (rd *Executor) result(ctx context.Context, rel string, x *agentv1.ReadArgs) (*agentv1.ReadResult, error) {
	path := rd.root.Abs(rel)
	st, err := rd.root.FS().Stat(rel)
	if pathroot.Escaped(err) {
		return nil, err
	}
	if err != nil {
		return readError(path, err), nil
	}
	if st.IsDir() {
		return invalidFile(path, msgDirectory), nil
	}
	b, err := rd.readAll(rel)
	if pathroot.Escaped(err) {
		return nil, err
	}
	if err != nil {
		return readError(path, err), nil
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	success := &agentv1.ReadSuccess{Path: path, FileSize: int64(len(b))}
	if slices.Contains(binaryExtensions, strings.ToLower(filepath.Ext(path))) {
		if len(b) > maxBinaryRead {
			return readError(path, errors.New("binary read exceeds JSON limit; use download")), nil
		}
		success.Output = &agentv1.ReadSuccess_Data{Data: b}
	} else {
		text, err := decodeFile(b, x.GetEncodingHint())
		if errors.Is(err, textcodec.ErrBinary) {
			return invalidFile(path, msgBinaryFile), nil
		}
		if err != nil {
			return invalidFile(path, err.Error()), nil
		}
		setTextRange(success, text, x)
	}
	return &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: success}}, nil
}

func (rd *Executor) readAll(rel string) ([]byte, error) {
	f, err := rd.root.OpenRegular(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, pathroot.MaxFile+1))
	if err == nil && len(b) > pathroot.MaxFile {
		err = pathroot.FileTooLarge(int64(len(b)))
	}
	return b, err
}
