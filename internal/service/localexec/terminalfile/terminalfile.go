// Package terminalfile writes the output files of background commands,
// <root>/terminals/<n>.txt, like the desktop's terminal logger (Fhe): a
// metadata header, rewritten in place every 5 s and when the status changes,
// the output decoded as UTF-8 and, once the command exits, a footer.
//
//	---
//	pid: 42
//	cwd: "/workspace"
//	command: "make"
//	title: "Build"
//	status: running
//	started_at: 2026-01-01T00:00:00.000Z
//	running_for_ms: 5004
//	---
//	<output>
//	---
//	exit_code: 0
//	elapsed_ms: 61234
//	ended_at: 2026-01-01T00:01:01.234Z
//	---
package terminalfile

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
)

// Dir is the directory of terminal files below the root.
const Dir = "terminals"

// The statuses of a command.
const (
	Running   = "running"
	Succeeded = "succeeded"
	Failed    = "failed"
	Aborted   = "aborted"
)

// MaxStream is how many bytes of each output stream a terminal file keeps,
// like the desktop's 1 MiB; a marker (gN) replaces the rest.
const MaxStream = 1 << 20

const (
	timestampFormat = "2006-01-02T15:04:05.000Z"
	// refresh is how often the header is rewritten (GVe).
	refresh = 5 * time.Second
)

// Command describes the command a terminal file belongs to; PID 0 is left
// out of the header, and so is a title of only whitespace.
type Command struct {
	PID     int
	Cwd     string
	Command string
	Title   string
	Started time.Time
}

// File is a terminal file being written. Output goes to the underlying file at
// its offset, for example from the command itself; the header is rewritten in
// place.
type File struct {
	f   *os.File
	cmd Command

	mu      sync.Mutex
	status  string
	ended   bool
	stop    chan struct{}
	streams []*stream
}

// StatusField is the status line without its newline, padded like the
// desktop's so it can be rewritten in place.
func StatusField(status string) string { return "status: " + padEnd(status) }

// Header returns the header of cmd with status, as of now (hN).
func Header(cmd Command, status string, now time.Time) string {
	var b strings.Builder
	b.WriteString("---\n")
	if cmd.PID != 0 {
		fmt.Fprintf(&b, "pid: %d\n", cmd.PID)
	}
	fmt.Fprintf(&b, "cwd: %s\n", quote(cmd.Cwd))
	fmt.Fprintf(&b, "command: %s\n", quote(cmd.Command))
	if strings.TrimSpace(cmd.Title) != "" {
		fmt.Fprintf(&b, "title: %s\n", quote(cmd.Title))
	}
	b.WriteString(StatusField(status) + "\n")
	fmt.Fprintf(&b, "started_at: %s\n", cmd.Started.UTC().Format(timestampFormat))
	fmt.Fprintf(&b, "running_for_ms: %s\n", padEnd(fmt.Sprint(now.Sub(cmd.Started).Milliseconds())))
	b.WriteString("---\n")
	return b.String()
}

// Start writes the header of cmd and initial output to f, a new or truncated
// file, and keeps the header up to date until Exit.
func Start(f *os.File, cmd Command, initial string) (*File, error) {
	t := &File{f: f, cmd: cmd, status: Running, stop: make(chan struct{})}
	if _, err := f.WriteString(Header(cmd, Running, time.Now()) + initial); err != nil {
		return nil, err
	}
	go t.refresh()
	return t, nil
}

// WriteText appends output that is already decoded; it is dropped after
// Exit.
func (t *File) WriteText(text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.write(text)
}

// Truncated appends the desktop's marker for stream name, "stdout" or
// "stderr", cut at MaxStream.
func (t *File) Truncated(name string) { t.WriteText(truncated(name)) }

// Stream returns the writer of output stream name, "stdout" or "stderr". Like
// the desktop's shell core executor, it decodes the output as UTF-8, with
// U+FFFD for invalid bytes and a sequence cut at the end of a write held back,
// and keeps MaxStream bytes: the write that would exceed them is replaced by
// the marker and the rest is dropped. Writes after Exit are dropped; held
// back bytes are written by Exit. Writes never fail, so the command never
// blocks on its output.
func (t *File) Stream(name string) io.Writer {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := &stream{t: t, name: name}
	t.streams = append(t.streams, s)
	return s
}

type stream struct {
	t       *File
	name    string
	written int
	trimmed bool
	partial []byte
}

func (s *stream) Write(b []byte) (int, error) {
	s.t.mu.Lock()
	defer s.t.mu.Unlock()
	if s.t.ended || s.trimmed {
		return len(b), nil
	}
	if s.written+len(b) > MaxStream {
		s.trimmed = true
		s.flush()
		s.t.write(truncated(s.name))
		return len(b), nil
	}
	s.written += len(b)
	data := append(s.partial, b...)
	n := textcodec.CompleteLen(data)
	s.partial = append([]byte(nil), data[n:]...)
	s.t.write(textcodec.LossyUTF8(data[:n]))
	return len(b), nil
}

// flush writes the held back bytes, as the desktop ends its decoder.
func (s *stream) flush() {
	if len(s.partial) > 0 {
		s.t.write(textcodec.LossyUTF8(s.partial))
	}
	s.partial = nil
}

// Code is the exit code of a command that ended with state for the footer:
// "unknown" for a command killed by a signal or that never ran.
func Code(state *os.ProcessState) string {
	if state == nil {
		return "unknown"
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return "unknown"
	}
	return fmt.Sprint(state.ExitCode())
}

// Exit writes the footer for a command that ended with exit code code (see
// Code), aborted by us or not, sets the final status and closes the file.
// Like the desktop, the status is aborted for an aborted command, else
// succeeded for exit code 0, else failed.
func (t *File) Exit(code string, aborted bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.streams {
		if !s.trimmed {
			s.flush()
		}
	}
	t.end()
	defer func() {
		if err := t.f.Close(); err != nil {
			log.Printf("terminal file: %v", err)
		}
	}()
	switch {
	case aborted:
		t.status = Aborted
	case code == "0":
		t.status = Succeeded
	default:
		t.status = Failed
	}
	now := time.Now()
	footer := fmt.Sprintf("\n---\nexit_code: %s\nelapsed_ms: %d\nended_at: %s\n---\n",
		code, now.Sub(t.cmd.Started).Milliseconds(), now.UTC().Format(timestampFormat))
	if _, err := t.f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if _, err := t.f.WriteString(footer); err != nil {
		return err
	}
	return t.writeHeader(now)
}

// Fail writes the footer for a command that could not be started, with the
// reason as the desktop's "error" field, sets the status to failed and
// closes the file.
func (t *File) Fail(reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.end()
	defer func() {
		if err := t.f.Close(); err != nil {
			log.Printf("terminal file: %v", err)
		}
	}()
	t.status = Failed
	now := time.Now()
	footer := fmt.Sprintf("\n---\nerror: %s\nelapsed_ms: %d\nended_at: %s\n---\n",
		quote(reason), now.Sub(t.cmd.Started).Milliseconds(), now.UTC().Format(timestampFormat))
	if _, err := t.f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if _, err := t.f.WriteString(footer); err != nil {
		return err
	}
	return t.writeHeader(now)
}

// Stop stops writing to a file whose command never ran; the caller closes it.
func (t *File) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.end()
}

func (t *File) write(text string) {
	if t.ended || text == "" {
		return
	}
	if _, err := t.f.WriteString(text); err != nil {
		log.Printf("terminal file: %v", err)
	}
}

func (t *File) end() {
	if !t.ended {
		t.ended = true
		close(t.stop)
	}
}

// refresh rewrites the header every refresh until Exit.
func (t *File) refresh() {
	tick := time.NewTicker(refresh)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case now := <-tick.C:
			t.mu.Lock()
			if !t.ended {
				if err := t.writeHeader(now); err != nil {
					log.Printf("terminal file: %v", err)
				}
			}
			t.mu.Unlock()
		}
	}
}

// writeHeader rewrites the header at the start of the file. Like the desktop,
// a running time beyond the padding, after 11 days, makes it longer than the
// one it overwrites.
func (t *File) writeHeader(now time.Time) error {
	_, err := t.f.WriteAt([]byte(Header(t.cmd, t.status, now)), 0)
	return err
}

// quote quotes s like the desktop's SN: only backslash, double quote,
// newline, carriage return and tab are escaped.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s) + `"`
}

// truncated is the desktop's marker for stream name cut at MaxStream (gN).
func truncated(name string) string {
	return fmt.Sprintf("\n[%s truncated by cursor: the command wrote more than %d characters to %s; "+
		"only the first %d were captured and the rest was dropped from this file]\n", name, MaxStream, name, MaxStream)
}

// padEnd pads s to the desktop's field width of 9.
func padEnd(s string) string { return fmt.Sprintf("%-9s", s) }
