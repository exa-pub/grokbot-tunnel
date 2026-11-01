// Package read reads a file for the agent, like the desktop's local-exec read
// executor (LocalReadExecutor): typed results for missing, unreadable and
// invalid files, images, line ranges, max_chars and text decoding.
package read

import (
	"context"
	"errors"
	"io"
	"math"
	"os"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
)

const (
	// envelopeReserve leaves room in execstream.MaxJSON for the rest of a
	// result message.
	envelopeReserve = 4096
	maxTextRead     = execstream.MaxJSON - envelopeReserve
	// Binary data is base64-encoded in JSON.
	maxBinaryRead = maxTextRead * 3 / 4
)

// Executor reads files below a root.
type Executor struct {
	root *pathroot.Root
}

// New returns an Executor for files below root.
func New(root *pathroot.Root) *Executor { return &Executor{root: root} }

// Execute reads args.Path and sends the read result. A path outside the root
// fails the request instead of producing a result, as on the desktop; below
// a root directory that cannot be opened, the path fails to open.
func (rd *Executor) Execute(ctx context.Context, x *agentv1.ReadArgs, send execstream.Send) error {
	if isBlank(x.Path) {
		return send(readMessage(&agentv1.ReadResult{Result: &agentv1.ReadResult_Error{
			Error: &agentv1.ReadError{Path: x.Path, Error: msgEmptyPath},
		}}))
	}
	p, err := rd.root.Check(x.Path)
	var unavailable *pathroot.RootError
	if errors.As(err, &unavailable) {
		return send(readMessage(readError(unavailable.Path, unavailable.Err)))
	}
	if err != nil {
		return err
	}
	defer p.Close()
	result, err := rd.result(ctx, p, x)
	if err != nil {
		return err
	}
	return send(readMessage(result))
}

// result reads a file into a typed result, deciding like the desktop: image,
// PDF, video, other binary data, text. Only failures that the desktop
// reports as errors rather than results are returned as errors.
func (rd *Executor) result(ctx context.Context, p *pathroot.Path, x *agentv1.ReadArgs) (*agentv1.ReadResult, error) {
	path := rd.root.Abs(p.Rel)
	if refused, err := rd.check(p, path); refused != nil || err != nil {
		return refused, err
	}
	f, err := p.OpenRegular()
	if pathroot.Escaped(err) {
		return nil, err
	}
	if err != nil {
		return readError(path, err), nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return readError(path, err), nil
	}
	head, err := readAt(f, textcodec.SniffLen)
	if err != nil {
		return readError(path, err), nil
	}
	format := textcodec.Detect(head)
	success := &agentv1.ReadSuccess{Path: path, FileSize: min(st.Size(), math.MaxUint32)}
	switch {
	case format.Image:
		return rd.image(ctx, f, path, success)
	case extname(path) == ".pdf", format.Binary && isVideo(path):
		data, err := readAll(f)
		if err != nil {
			return readError(path, err), nil
		}
		return dataResult(path, success, data), nil
	case format.Binary:
		return invalidFile(path, msgBinaryFile), nil
	}
	res, err := readText(ctx, f, st.Size(), format.Encoding, x)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return readError(path, err), nil
	}
	content, truncated := contentString(res.content)
	success.Output = &agentv1.ReadSuccess_Content{Content: content}
	success.TotalLines = int32(min(res.totalLines, math.MaxInt32)) //nolint:gosec // clamped
	success.Truncated = truncated
	success.RangeApplied = res.rangeApplied
	return &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: success}}, nil
}

// check returns the result for a path that is not a readable file by its
// stat: missing, a directory, neither file nor directory, or too large.
func (rd *Executor) check(p *pathroot.Path, path string) (*agentv1.ReadResult, error) {
	st, err := p.FS().Stat(p.Real)
	switch {
	case pathroot.Escaped(err):
		return nil, err
	case err != nil:
		return readError(path, err), nil
	case st.IsDir():
		return invalidFile(path, msgDirectory), nil
	case !st.Mode().IsRegular():
		return invalidFile(path, msgNotFile), nil
	case st.Size() > pathroot.MaxFile:
		return readError(path, pathroot.FileTooLarge(st.Size())), nil
	}
	return nil, nil //nolint:nilnil // a readable file
}

// image returns an image as the desktop prepares it for the model.
func (rd *Executor) image(ctx context.Context, f *os.File, path string, success *agentv1.ReadSuccess) (*agentv1.ReadResult, error) {
	data, err := readAll(f)
	if err != nil {
		return readError(path, err), nil
	}
	img, err := prepareImage(ctx, data)
	var undecodable *undecodableError
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.As(err, &undecodable):
		return invalidFile(path, undecodable.Reason()), nil
	case err != nil:
		return readError(path, err), nil
	}
	return dataResult(path, success, img), nil
}

func dataResult(path string, success *agentv1.ReadSuccess, data []byte) *agentv1.ReadResult {
	if len(data) > maxBinaryRead {
		return readError(path, errors.New("binary read exceeds JSON limit; use download"))
	}
	success.Output = &agentv1.ReadSuccess_Data{Data: data}
	return &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: success}}
}

// readText reads text as the desktop's streaming reader does (m_r): with the
// hinted or detected encoding, and again as lossy UTF-8 or Latin-1 if strict
// UTF-8 meets invalid data.
func readText(ctx context.Context, f *os.File, size int64, encoding string, x *agentv1.ReadArgs) (textResult, error) {
	res, err := streamText(ctx, f, size, textcodec.Resolve(x.EncodingHint, encoding), x)
	if !errors.Is(err, textcodec.ErrInvalidUTF8) {
		return res, err
	}
	n := int64(textcodec.FallbackSniff)
	if size > 0 {
		n = min(size, n)
	}
	head, err := readAt(f, int(n))
	if err != nil {
		return textResult{}, err
	}
	return streamText(ctx, f, size, textcodec.Fallback(head, size), x)
}

// streamText reads the file in 64 KiB chunks up to its size, or to the end
// if its size is 0, keeping only what the read returns.
func streamText(ctx context.Context, f *os.File, size int64, d textcodec.Decoding, x *agentv1.ReadArgs) (textResult, error) {
	limit := int64(math.MaxInt64)
	if size > 0 {
		limit = size
	}
	src := io.NewSectionReader(f, 0, limit)
	r := newTextReader(x)
	dec := d.NewDecoder()
	buf := make([]byte, readChunk)
	for {
		if err := ctx.Err(); err != nil {
			return textResult{}, err
		}
		n, err := src.Read(buf)
		if n > 0 {
			out, decErr := dec.Decode(buf[:n], false)
			if decErr != nil {
				return textResult{}, decErr
			}
			r.chunk(out)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return textResult{}, err
		}
	}
	out, err := dec.Decode(nil, true)
	if err != nil {
		return textResult{}, err
	}
	r.chunk(out)
	return r.finish(), nil
}

// readAt reads up to n bytes from the start of f.
func readAt(f *os.File, n int) ([]byte, error) {
	b := make([]byte, n)
	m, err := f.ReadAt(b, 0)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return b[:m], err
}

func readAll(f *os.File) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(io.NewSectionReader(f, 0, math.MaxInt64), pathroot.MaxFile+1))
	if err == nil && len(b) > pathroot.MaxFile {
		err = pathroot.FileTooLarge(int64(len(b)))
	}
	return b, err
}
