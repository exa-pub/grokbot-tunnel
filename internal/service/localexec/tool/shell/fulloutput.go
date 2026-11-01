package shell

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
)

const (
	// fullOutputDir is the desktop's directory of full outputs below the
	// project, which on local-exec is the root (oN).
	fullOutputDir = "agent-tools"
	// maxFullOutput is how many bytes of output a full output keeps, like the
	// desktop's 50 MiB less 1 MiB (YE).
	maxFullOutput = 50<<20 - 1<<20
)

// fullOutput keeps the whole output of a command that asked for it with
// fileOutputThresholdBytes, like the desktop's shell core executor: stdout and
// stderr as they arrive, decoded, in memory up to the threshold and from then
// on in <root>/agent-tools/<uuid>.txt, which the exit reports. Methods on a
// nil fullOutput do nothing. The caller serializes them.
type fullOutput struct {
	root      *pathroot.Root
	threshold uint64
	buffer    strings.Builder
	size      int64 // bytes of output kept
	lines     int64 // newlines in the output kept
	truncated bool  // maxFullOutput was reached, or the file failed
	path      string
	file      *os.File
	err       error // the first failure to save the output
}

// live reports whether output is still kept.
func (f *fullOutput) live() bool { return f != nil && !f.truncated }

// fits reports whether n more bytes of output can be kept.
func (f *fullOutput) fits(n int) bool { return f.size+int64(n) <= maxFullOutput }

// take keeps text, decoded from output b, if keep, else stops keeping the
// output.
func (f *fullOutput) take(keep bool, b []byte, text string) {
	switch {
	case keep:
		f.add(len(b), bytes.Count(b, []byte{'\n'}), text)
	case f != nil:
		f.truncated = true
	}
}

// add keeps text, decoded from n bytes of output with lines newlines, and
// moves the output to its file once it exceeds the threshold.
func (f *fullOutput) add(n, lines int, text string) {
	f.size += int64(n)
	f.lines += int64(lines)
	if f.file != nil {
		f.write(text)
		return
	}
	f.buffer.WriteString(text)
	if uint64(f.size) > f.threshold { //nolint:gosec // size is not negative
		f.create()
	}
}

// write appends text without counting it.
func (f *fullOutput) write(text string) {
	if f == nil || text == "" {
		return
	}
	if f.file == nil {
		f.buffer.WriteString(text)
		return
	}
	if _, err := f.file.WriteString(text); err != nil && f.err == nil {
		f.err = err
	}
}

func (f *fullOutput) create() {
	name := newUUID() + ".txt"
	f.path = filepath.Join(f.root.Dir(), fullOutputDir, name)
	defer f.buffer.Reset()
	if err := f.root.MkdirAll(fullOutputDir, 0o700); err != nil {
		f.err, f.truncated = err, true
		return
	}
	file, err := f.root.OpenFile(fullOutputDir+"/"+name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		f.err, f.truncated = err, true
		return
	}
	f.file = file
	f.write(f.buffer.String())
}

// close ends the full output once the command has exited: the output
// location, if the output went to a file, and a message for stderr if it
// could not be saved, as on the desktop. A file cut at maxFullOutput ends with
// a note.
func (f *fullOutput) close() (*agentv1.OutputLocation, string) {
	if f == nil {
		return nil, ""
	}
	var location *agentv1.OutputLocation
	if f.file != nil {
		if f.truncated {
			note := fmt.Sprintf("\n... (output truncated at %d bytes) ...\n", f.size)
			f.write(note)
			f.size += int64(len(note))
			f.lines += 2
		}
		if err := f.file.Close(); err != nil && f.err == nil {
			f.err = err
		}
		f.file = nil
		if f.err != nil {
			proc.RemoveLogged(f.path)
		} else {
			location = &agentv1.OutputLocation{FilePath: f.path, SizeBytes: f.size, LineCount: f.lines}
		}
	}
	if f.err != nil && f.path != "" {
		return nil, fmt.Sprintf("\nThe full output could not be saved to %s: %v\n", f.path, f.err)
	}
	return location, ""
}

// release closes the file of a command whose exit is not reported.
func (f *fullOutput) release() {
	if f != nil && f.file != nil {
		proc.CloseLogged(f.file)
		f.file = nil
	}
}

// newUUID returns a random UUID, like Node's randomUUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
