package shell

import (
	"sync"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/terminalfile"
)

func shellMessage(s *agentv1.ShellStream) *agentv1.ExecClientMessage {
	return &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: s}}
}

func stdoutEvent(data string) *agentv1.ShellStream {
	return &agentv1.ShellStream{Event: &agentv1.ShellStream_Stdout{Stdout: &agentv1.ShellStreamStdout{Data: data}}}
}

func stderrEvent(data string) *agentv1.ShellStream {
	return &agentv1.ShellStream{Event: &agentv1.ShellStream_Stderr{Stderr: &agentv1.ShellStreamStderr{Data: data}}}
}

// shellOutput streams stdout and stderr to the agent and, once the command is
// detached, to a terminal file, and keeps the full output if asked to. A
// single mutex preserves frame ordering across both streams.
type shellOutput struct {
	mu       sync.Mutex
	send     execstream.Send
	hold     bool   // keep the output for a terminal file, the command may be detached
	held     []byte // output sent so far, while hold
	file     *terminalfile.File
	shellID  uint32 // number of the terminal file
	detached bool   // the command was backgrounded; stop streaming
	finished bool   // the command has exited; discard further output
	err      error  // first send failure
	full     *fullOutput
}

// detach starts terminal file shellID with start, given the output so far,
// and stops streaming to the agent. It reports false, without calling start,
// if the command has already finished.
func (o *shellOutput) detach(shellID uint32, start func(held string) (*terminalfile.File, error)) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return false, nil
	}
	f, err := start(string(o.held))
	if err != nil {
		return false, err
	}
	o.file, o.shellID = f, shellID
	o.hold, o.held, o.detached = false, nil, true
	return true, nil
}

// emit forwards decoded output of a stream to the agent or, once detached,
// to the terminal file.
func (o *shellOutput) emit(event func(string) *agentv1.ShellStream, text string) {
	if text == "" {
		return
	}
	if o.detached {
		o.file.WriteText(text)
		return
	}
	if o.hold {
		o.held = append(o.held, text...)
	}
	for data := []byte(text); len(data) > 0 && o.err == nil; {
		n := textcodec.ChunkLen(data, streamChunk)
		o.err = o.send(shellMessage(event(string(data[:n]))))
		data = data[n:]
	}
}

// finish discards all further output, so that nothing reaches the agent after
// the command's exit or the terminal file after its footer. It returns the
// terminal file, if the command was detached, for the footer, and the full
// output.
func (o *shellOutput) finish() (*terminalfile.File, uint32, *fullOutput) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.finished = true
	o.held = nil
	return o.file, o.shellID, o.full
}

func (o *shellOutput) sendErr() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// maxStreamOutput is how many bytes of a stream the desktop forwards; the
// agent gets nothing of a stream after the write that would exceed it. A
// terminal file keeps the same.
const maxStreamOutput = terminalfile.MaxStream

// streamWriter forwards one stream like the desktop's shell core executor
// (dS): decoded as UTF-8, with a trailing incomplete sequence held back until
// the rest of it arrives, and cut at maxStreamOutput. The full output, if
// kept, goes on after the cut.
type streamWriter struct {
	out     *shellOutput
	name    string                            // "stdout" or "stderr"
	event   func(string) *agentv1.ShellStream // stdoutEvent or stderrEvent
	partial []byte
	sent    int  // bytes of the stream forwarded so far
	trimmed bool // maxStreamOutput was reached
}

// Write never fails: the process must keep running even if the agent or the
// terminal file stops accepting output, otherwise it would block on a full
// pipe.
func (w *streamWriter) Write(b []byte) (int, error) {
	o := w.out
	o.mu.Lock()
	defer o.mu.Unlock()
	full := o.full
	if o.finished || w.trimmed && !full.live() {
		return len(b), nil
	}
	forward := !w.trimmed && w.sent+len(b) <= maxStreamOutput
	keep := full.live() && full.fits(len(b))
	cut := !w.trimmed && !forward
	held := w.flushHeld(cut, full.live() && !keep)
	text := w.decode(b, forward || keep)
	full.take(keep, b, text)
	switch {
	case forward:
		w.sent += len(b)
		o.emit(w.event, text)
	case cut:
		o.emit(w.event, held)
		w.trimmed = true
		if o.detached {
			o.file.Truncated(w.name)
		}
	}
	return len(b), nil
}

// flushHeld returns the held-back bytes, decoded, if the stream is cut now,
// and writes them to the full output if that stops now.
func (w *streamWriter) flushHeld(cut, stop bool) string {
	if !cut && !stop {
		return ""
	}
	held := textcodec.LossyUTF8(w.partial)
	if stop {
		w.out.full.write(held)
	}
	if !cut {
		return ""
	}
	return held
}

// decode returns b decoded after the held-back bytes, holding back a trailing
// incomplete sequence, or drops the held-back bytes if b is not used.
func (w *streamWriter) decode(b []byte, used bool) string {
	if !used {
		w.partial = nil
		return ""
	}
	data := append(w.partial, b...)
	n := textcodec.CompleteLen(data)
	w.partial = append([]byte(nil), data[n:]...)
	return textcodec.LossyUTF8(data[:n])
}

// flush writes the held-back bytes, as the desktop ends its decoder when the
// command exits.
func (w *streamWriter) flush() {
	o := w.out
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(w.partial) > 0 && !o.finished {
		text := textcodec.LossyUTF8(w.partial)
		if o.full.live() {
			o.full.write(text)
		}
		if !w.trimmed {
			o.emit(w.event, text)
		}
	}
	w.partial = nil
}
